package aws2

import (
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
)

type AWS struct {
	ecsService *ecs.ECS
	ec2Service *ec2.EC2

	describeTaskDefitionCache map[string]*ecs.DescribeTaskDefinitionOutput
	reservationsCache         map[string]*ec2.Reservation

	mutex sync.Mutex
}

func NewAWS(awsConfig *aws.Config) *AWS {
	aws := &AWS{
		ecsService:                ecs.New(session.New(), awsConfig),
		ec2Service:                ec2.New(session.New(), awsConfig),
		describeTaskDefitionCache: make(map[string]*ecs.DescribeTaskDefinitionOutput),
		reservationsCache:         make(map[string]*ec2.Reservation),
	}
	return aws
}

func (aws *AWS) GetTaskDefinition(taskDefinitionArn *string) (*ecs.TaskDefinition, error) {
	aws.mutex.Lock()
	defer aws.mutex.Unlock()
	var err error
	// In case we encounter more than one service using the same service definition, no need to look it up again
	describeTaskDefinitionResponse, cached := aws.describeTaskDefitionCache[*taskDefinitionArn]
	if !cached {
		describeTaskDefinitionResponse, err = aws.ecsService.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
			TaskDefinition: taskDefinitionArn,
		})
		if err != nil {
			return nil, fmt.Errorf("error describing ECS task definition: %s", err)
		}
		aws.describeTaskDefitionCache[*taskDefinitionArn] = describeTaskDefinitionResponse
	} else {
		log.Debugf("describeTaskDefitionCache HIT")
	}
	return describeTaskDefinitionResponse.TaskDefinition, nil
}

func (aws *AWS) GetClusters() ([]*ecs.Cluster, error) {
	listClustersResponse, err := aws.ecsService.ListClusters(
		&ecs.ListClustersInput{})
	if err != nil {
		return nil, fmt.Errorf("error listing ECS clusters: %s", err)
	}

	describeClustersResponse, err := aws.ecsService.DescribeClusters(
		&ecs.DescribeClustersInput{
			Clusters: listClustersResponse.ClusterArns,
		})
	if err != nil {
		return nil, fmt.Errorf("error describing ECS clusters: %s", err)
	}
	return describeClustersResponse.Clusters, nil
}

func (aws *AWS) GetServices(cluster *ecs.Cluster) ([]*ecs.Service, error) {
	listServicesResponse := &ecs.ListServicesOutput{}
	err := aws.ecsService.ListServicesPages(&ecs.ListServicesInput{
		Cluster: cluster.ClusterArn,
	}, func(page *ecs.ListServicesOutput, lastPage bool) bool {
		listServicesResponse.ServiceArns = append(listServicesResponse.ServiceArns, page.ServiceArns...)
		return !lastPage
	})
	if err != nil {
		return nil, fmt.Errorf("error listing ECS services: %s", err)
	}

	describeServicesResponse, err := aws.ecsService.DescribeServices(&ecs.DescribeServicesInput{
		Cluster:  cluster.ClusterArn,
		Services: listServicesResponse.ServiceArns,
	})
	if err != nil {
		return nil, fmt.Errorf("error describing ECS services: %s", err)
	}
	return describeServicesResponse.Services, nil
}

func (aws *AWS) GetInstances(cluster *ecs.Cluster) (map[string]*ec2.Instance, error) {
	aws.mutex.Lock()
	defer aws.mutex.Unlock()
	instances := make(map[string]*ec2.Instance)
	var err error
	var containerInstanceArns []*string
	aws.ecsService.ListContainerInstancesPages(&ecs.ListContainerInstancesInput{
		Cluster: cluster.ClusterArn,
	}, func(page *ecs.ListContainerInstancesOutput, lastPage bool) bool {
		containerInstanceArns = append(containerInstanceArns, page.ContainerInstanceArns...)
		return !lastPage
	})
	if err != nil {
		return instances, fmt.Errorf("error listing ECS container instances: %s", err)
	}

	if len(containerInstanceArns) == 0 {
		return instances, nil
	}

	describeContainerInstancesResponse, err := aws.ecsService.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster:            cluster.ClusterArn,
		ContainerInstances: containerInstanceArns,
	})
	if err != nil {
		return instances, fmt.Errorf("error describing ECS container instances: %s", err)
	}

	describeInstancesRequest := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{},
	}

	var reservations []*ec2.Reservation
	for _, containerInstance := range describeContainerInstancesResponse.ContainerInstances {
		reservation, cached := aws.reservationsCache[*containerInstance.Ec2InstanceId]
		if !cached {
			describeInstancesRequest.InstanceIds = append(describeInstancesRequest.InstanceIds, containerInstance.Ec2InstanceId)
		} else {
			reservations = append(reservations, reservation)
			log.Debugf("reservationsCache HIT")
		}
	}

	if len(describeInstancesRequest.InstanceIds) != 0 {
		err = aws.ec2Service.DescribeInstancesPages(
			describeInstancesRequest,
			func(page *ec2.DescribeInstancesOutput, lastPage bool) bool {
				reservations = append(reservations, page.Reservations...)
				return !lastPage
			})
		if err != nil {
			return instances, fmt.Errorf("error describing EC2 instance: %s", err)
		}
	}

	for _, reservation := range reservations {
		for _, ec2 := range reservation.Instances {
			aws.reservationsCache[*ec2.InstanceId] = reservation
			for _, containerInstance := range describeContainerInstancesResponse.ContainerInstances {
				if *containerInstance.Ec2InstanceId == *ec2.InstanceId {
					instances[*containerInstance.ContainerInstanceArn] = ec2
					continue
				}
			}
		}
	}
	return instances, nil
}

func (aws *AWS) GetTasks(cluster *ecs.Cluster) (tasks []*ecs.Task, err error) {
	var taskArns []*string
	err = aws.ecsService.ListTasksPages(&ecs.ListTasksInput{
		Cluster: cluster.ClusterArn,
	}, func(page *ecs.ListTasksOutput, lastPage bool) bool {
		taskArns = append(taskArns, page.TaskArns...)
		return !lastPage
	})
	if err != nil {
		return nil, fmt.Errorf("error listing ECS tasks: %s", err)
	}

	if len(taskArns) == 0 {
		return
	}

	taskDescriptionsResponse, err := aws.ecsService.DescribeTasks(&ecs.DescribeTasksInput{
		Cluster: cluster.ClusterArn,
		Tasks:   taskArns,
	})
	if err != nil {
		return nil, fmt.Errorf("error describing ECS tasks: %s", err)
	}
	return taskDescriptionsResponse.Tasks, nil
}
