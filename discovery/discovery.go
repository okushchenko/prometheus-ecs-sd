package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	awsService "github.com/okushchenko/prometheus-ecs-sd/aws"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/youtube/vitess/go/ioutil2"
)

const (
	ecsMetaPrefix     = model.MetaLabelPrefix + "ecs_"
	ecsServiceLabel   = ecsMetaPrefix + "service_name"
	ecsClusterLabel   = ecsMetaPrefix + "cluster_name"
	ecsContainerLabel = ecsMetaPrefix + "container_name"
	ecsTaskLabel      = ecsMetaPrefix + "task_arn"
	ecsVersionLabel   = ecsMetaPrefix + "task_version"
	ecsInstanceLabel  = ecsMetaPrefix + "instance_id"
	ecsLabelPrefix    = ecsMetaPrefix + "label_"
)

// TargetGroup is a part of output for file SD in Prometheus
type TargetGroup struct {
	Targets []string          `json:"targets,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// ECSDiscovery periodically performs ECS-SD requests. It implements
// the TargetProvider interface.
type ECSDiscovery struct {
	aws      *awsService.AWS
	interval time.Duration
	path     string
}

type Config struct {
	Interval time.Duration
	Region   string
	Path     string
}

// NewECSDiscovery returns a new ECSDiscovery which periodically refreshes its targets.
func NewECSDiscovery(config Config) *ECSDiscovery {
	awsConfig := defaults.Get().Config
	awsConfig.Region = aws.String(config.Region)

	return &ECSDiscovery{
		aws:      awsService.NewAWS(awsConfig),
		interval: config.Interval,
		path:     config.Path,
	}
}

// Run implements the TargetProvider interface.
func (ed *ECSDiscovery) Run(ctx context.Context) {
	ticker := time.NewTicker(ed.interval)
	defer ticker.Stop()

	p := func() {
		targetGroups, err := ed.refresh()
		if err != nil {
			log.Fatal(err)
		}
		targetGroupsMarshalled, err := json.MarshalIndent(targetGroups, "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		log.Debugf("Writing to %s", ed.path)
		err = ioutil2.WriteFileAtomic(ed.path, targetGroupsMarshalled, 0644)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Get an initial set right away.
	p()

	for {
		select {
		case <-ticker.C:
			p()
		case <-ctx.Done():
			return
		}
	}
}

func (ed *ECSDiscovery) refresh() (*[]TargetGroup, error) {
	t0 := time.Now()
	targetGroups := []TargetGroup{}

	clusters, err := ed.aws.GetClusters()
	if err != nil {
		return nil, err
	}

	out := make(chan TargetGroup)
	var wg sync.WaitGroup
	for _, cluster := range clusters {
		log.Debugf("Processing cluster %s", *cluster.ClusterName)
		if *cluster.ActiveServicesCount == 0 {
			log.Debugf("Cluster %s has no active services, skipping", *cluster.ClusterName)
			continue
		}
		if *cluster.RegisteredContainerInstancesCount == 0 {
			log.Debugf("Cluster %s has no registered container instances, skipping", *cluster.ClusterName)
			continue
		}
		if *cluster.RunningTasksCount == 0 {
			log.Debugf("Cluster %s has no running tasks, skipping", *cluster.ClusterName)
			continue
		}
		if *cluster.Status == "INACTIVE" {
			log.Debugf("Cluster %s is in the INACTIVE state, skipping", *cluster.ClusterName)
			continue
		}
		wg.Add(1)
		go func(cluster *ecs.Cluster) {
			defer wg.Done()

			instances, err := ed.aws.GetInstances(cluster)
			if err != nil {
				log.Fatal(err)
			}

			tasks, err := ed.aws.GetTasks(cluster)
			if err != nil {
				log.Fatal(err)
			}

			for _, task := range tasks {
				log.Debugf("Processing task %s from %s", *task.TaskArn, *task.Group)
				if !strings.HasPrefix(*task.Group, "service:") {
					log.Debugf("Task %s is not a part of a service", *task.TaskArn)
					continue
				}
				service := strings.TrimPrefix(*task.Group, "service:")
				ec2Instance := instances[*task.ContainerInstanceArn]
				taskDefinition, err := ed.aws.GetTaskDefinition(task.TaskDefinitionArn)
				if err != nil {
					log.Fatal(err)
				}
				for _, container := range task.Containers {
					// Skip containers with no exposed ports or not in a RUNNING status
					if len(container.NetworkBindings) == 0 || *container.LastStatus != "RUNNING" {
						continue
					}
					wg.Add(1)
					go func(
						container *ecs.Container, task *ecs.Task, service *string,
						cluster *ecs.Cluster, ec2Instance *ec2.Instance, taskDefinition *ecs.TaskDefinition) {
						defer wg.Done()
						out <- *processContainer(container, task, service, cluster, ec2Instance, taskDefinition)
					}(container, task, &service, cluster, ec2Instance, taskDefinition)
				}
			}
		}(cluster)
	}

	go func() {
		for targetGroup := range out {
			targetGroups = append(targetGroups, targetGroup)
		}
	}()

	wg.Wait()
	close(out)
	log.Infof("Refresh took %s", time.Since(t0))
	return &targetGroups, nil
}

func processContainer(
	container *ecs.Container,
	task *ecs.Task,
	service *string,
	cluster *ecs.Cluster,
	ec2Instance *ec2.Instance,
	taskDefinition *ecs.TaskDefinition) *TargetGroup {
	targetGroup := &TargetGroup{
		Labels: map[string]string{
			ecsServiceLabel:   *service,
			ecsClusterLabel:   *cluster.ClusterName,
			ecsContainerLabel: *container.Name,
			ecsTaskLabel:      *container.TaskArn,
			ecsInstanceLabel:  *ec2Instance.InstanceId,
			ecsVersionLabel:   strconv.FormatInt(*task.Version, 10),
		},
		Targets: []string{fmt.Sprintf(
			"%s:%d",
			*ec2Instance.PrivateIpAddress,
			*container.NetworkBindings[0].HostPort)},
	}

	for _, containerDefinition := range taskDefinition.ContainerDefinitions {
		if *containerDefinition.Name == *container.Name {
			for k, v := range containerDefinition.DockerLabels {
				targetGroup.Labels[strutil.SanitizeLabelName(ecsLabelPrefix+k)] = *v
			}
		}
	}
	return targetGroup
}
