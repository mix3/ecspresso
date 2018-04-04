package main

import (
	"log"
	"os"

	"github.com/kayac/ecspresso"
	config "github.com/kayac/go-config"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

func main() {
	os.Exit(_main())
}

func _main() int {
	conf := kingpin.Flag("config", "config file").Required().String()

	deploy := kingpin.Command("deploy", "deploy service")
	deployOption := ecspresso.DeployOption{
		DryRun:             deploy.Flag("dry-run", "dry-run").Bool(),
		DesiredCount:       deploy.Flag("tasks", "desired count of tasks").Default("-1").Int64(),
		SkipTaskDefinition: deploy.Flag("skip-task-definition", "skip register a new task definition").Bool(),
		ForceNewDeployment: deploy.Flag("force-new-deployment", "force a new deployment of the service").Bool(),
	}

	create := kingpin.Command("create", "create service")
	createOption := ecspresso.CreateOption{
		DryRun:       create.Flag("dry-run", "dry-run").Bool(),
		DesiredCount: create.Flag("tasks", "desired count of tasks").Default("1").Int64(),
	}

	status := kingpin.Command("status", "show status of service")
	statusOption := ecspresso.StatusOption{
		Events: status.Flag("events", "show events num").Default("2").Int(),
	}

	rollback := kingpin.Command("rollback", "rollback service")
	rollbackOption := ecspresso.RollbackOption{
		DryRun: rollback.Flag("dry-run", "dry-run").Bool(),
		DeregisterTaskDefinition: rollback.Flag("dereginster-task-definition", "deregister rolled back task definition").Bool(),
	}

	delete := kingpin.Command("delete", "delete service")
	deleteOption := ecspresso.DeleteOption{
		DryRun: delete.Flag("dry-run", "dry-run").Bool(),
		Force:  delete.Flag("force", "force delete. not confirm").Bool(),
	}

	taskCommand := kingpin.Command("task", "task")
	taskCreate := taskCommand.Command("create", "task create")
	taskCreateOption := ecspresso.TaskCreateOption{
		DryRun: taskCreate.Flag("dry-run", "dry-run").Bool(),
	}

	sub := kingpin.Parse()

	c := ecspresso.NewDefaultConfig()
	if err := config.Load(c, *conf); err != nil {
		log.Println("Cloud not load config file", conf, err)
		kingpin.Usage()
		return 1
	}

	var (
		app *ecspresso.App
		err error
	)
	switch sub {
	case "deploy", "status", "rollback", "create", "delete":
		app, err = ecspresso.NewApp(c)
		if err != nil {
			log.Println(err)
			return 1
		}

		switch sub {
		case "deploy":
			err = app.Deploy(deployOption)
		case "status":
			err = app.Status(statusOption)
		case "rollback":
			err = app.Rollback(rollbackOption)
		case "create":
			err = app.Create(createOption)
		case "delete":
			err = app.Delete(deleteOption)
		}
	case "task create":
		app, err = ecspresso.NewTaskApp(c)
		if err != nil {
			log.Println(err)
			return 1
		}

		switch sub {
		case "task create":
			err = app.TaskCreate(taskCreateOption)
		}
	default:
		kingpin.Usage()
		return 1
	}

	if err != nil {
		log.Printf("%s FAILED. %s", sub, err)
		return 1
	}

	return 0
}
