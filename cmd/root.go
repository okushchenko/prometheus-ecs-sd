package cmd

import (
	"context"
	"time"

	"github.com/okushchenko/prometheus-ecs-sd/discovery"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var RootCmd = &cobra.Command{
	Run: func(cmd *cobra.Command, args []string) {
		viper.SetEnvPrefix("ecssd")
		viper.SetDefault("interval", 10)
		viper.SetDefault("region", "us-east-1")
		viper.SetDefault("path", "prometheus-ecs-sd.json")
		viper.SetDefault("debug", false)
		viper.AutomaticEnv()
		if viper.GetBool("debug") {
			log.SetLevel(log.DebugLevel)
		}
		ctx := context.Background()
		discovery.NewECSDiscovery(discovery.Config{
			Interval: time.Duration(viper.GetInt("interval")) * time.Second,
			Region:   viper.GetString("region"),
			Path:     viper.GetString("path"),
		}).Run(ctx)
	},
}
