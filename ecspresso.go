package ecspresso

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Songmu/prompter"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/kayac/go-config"
	"github.com/mattn/go-isatty"
	"github.com/morikuni/aec"
	"github.com/pkg/errors"
)

var isTerminal = isatty.IsTerminal(os.Stdout.Fd())
var TerminalWidth = 90

const KeepDesiredCount = -1

func taskDefinitionName(t *ecs.TaskDefinition) string {
	return fmt.Sprintf("%s:%d", *t.Family, *t.Revision)
}

type App struct {
	ecs     *ecs.ECS
	Service string
	Cluster string
	config  *Config
}

func (d *App) DescribeServicesInput() *ecs.DescribeServicesInput {
	return &ecs.DescribeServicesInput{
		Cluster:  aws.String(d.Cluster),
		Services: []*string{aws.String(d.Service)},
	}
}

func (d *App) DescribeServiceStatus(ctx context.Context, events int) (*ecs.Service, error) {
	out, err := d.ecs.DescribeServicesWithContext(ctx, d.DescribeServicesInput())
	if err != nil {
		return nil, errors.Wrap(err, "describe services failed")
	}
	if len(out.Services) == 0 {
		return nil, errors.New("no services found")
	}
	s := out.Services[0]
	fmt.Println("Service:", *s.ServiceName)
	fmt.Println("Cluster:", arnToName(*s.ClusterArn))
	fmt.Println("TaskDefinition:", arnToName(*s.TaskDefinition))
	fmt.Println("Deployments:")
	for _, dep := range s.Deployments {
		fmt.Println("  ", formatDeployment(dep))
	}
	fmt.Println("Events:")
	for i, event := range s.Events {
		if i >= events {
			break
		}
		for _, line := range formatEvent(event, TerminalWidth) {
			fmt.Println(line)
		}
	}
	return s, nil
}

func (d *App) DescribeServiceDeployments(ctx context.Context, startedAt time.Time) (int, error) {
	out, err := d.ecs.DescribeServicesWithContext(ctx, d.DescribeServicesInput())
	if err != nil {
		return 0, err
	}
	if len(out.Services) == 0 {
		return 0, nil
	}
	s := out.Services[0]
	lines := 0
	for _, dep := range s.Deployments {
		lines++
		d.Log(formatDeployment(dep))
	}
	for _, event := range s.Events {
		if (*event.CreatedAt).After(startedAt) {
			for _, line := range formatEvent(event, TerminalWidth) {
				fmt.Println(line)
				lines++
			}
		}
	}
	return lines, nil
}

func NewApp(conf *Config) (*App, error) {
	if err := conf.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid configuration")
	}
	sess := session.Must(session.NewSession(
		&aws.Config{Region: aws.String(conf.Region)},
	))
	d := &App{
		Service: conf.Service,
		Cluster: conf.Cluster,
		ecs:     ecs.New(sess),
		config:  conf,
	}
	return d, nil
}

func (d *App) Start() (context.Context, context.CancelFunc) {
	log.SetOutput(os.Stdout)

	if d.config.Timeout > 0 {
		return context.WithTimeout(context.Background(), d.config.Timeout)
	} else {
		return context.Background(), func() {}
	}
}

func (d *App) Status(opt StatusOption) error {
	ctx, cancel := d.Start()
	defer cancel()
	_, err := d.DescribeServiceStatus(ctx, *opt.Events)
	return err
}

func (d *App) Create(opt CreateOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting create service")
	td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "create failed")
	}
	svd, err := d.LoadServiceDefinition(d.config.ServiceDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "create failed")
	}

	if *opt.DesiredCount != 1 {
		svd.DesiredCount = opt.DesiredCount
	}

	if *opt.DryRun {
		d.Log("task definition:", td.String())
		d.Log("service definition:", svd.String())
		d.Log("DRY RUN OK")
		return nil
	}

	newTd, err := d.RegisterTaskDefinition(ctx, td)
	if err != nil {
		return errors.Wrap(err, "create failed")
	}
	svd.TaskDefinition = newTd.TaskDefinitionArn

	if _, err := d.ecs.CreateServiceWithContext(ctx, svd); err != nil {
		return errors.Wrap(err, "create failed")
	}
	d.Log("Service is created")

	start := time.Now()
	time.Sleep(3 * time.Second) // wait for service created
	if err := d.WaitServiceStable(ctx, start); err != nil {
		return errors.Wrap(err, "create failed")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) Delete(opt DeleteOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Deleting service")
	sv, err := d.DescribeServiceStatus(ctx, 3)
	if err != nil {
		return err
	}

	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	if !*opt.Force {
		service := prompter.Prompt(`Enter the service name to DELETE`, "")
		if service != *sv.ServiceName {
			d.Log("Aborted")
			return errors.New("confirmation failed")
		}
	}

	dsi := &ecs.DeleteServiceInput{
		Cluster: sv.ClusterArn,
		Service: sv.ServiceName,
	}
	if _, err := d.ecs.DeleteServiceWithContext(ctx, dsi); err != nil {
		return errors.Wrap(err, "delete failed")
	}
	d.Log("Service is deleted")

	return nil
}

func (d *App) Deploy(opt DeployOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting deploy")
	svd, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "deploy failed")
	}

	var count *int64
	if *opt.DesiredCount == KeepDesiredCount {
		count = svd.DesiredCount
	} else {
		count = opt.DesiredCount
	}

	var tdArn string
	if *opt.SkipTaskDefinition {
		tdArn = *svd.TaskDefinition
	} else {
		td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "deploy failed")
		}
		if *opt.DryRun {
			d.Log("task definition:", td.String())
		} else {
			newTd, err := d.RegisterTaskDefinition(ctx, td)
			if err != nil {
				return errors.Wrap(err, "deploy failed")
			}
			tdArn = *newTd.TaskDefinitionArn
		}
	}
	d.Log("desired count:", *count)
	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	if err := d.UpdateService(ctx, tdArn, *count, *opt.ForceNewDeployment); err != nil {
		return errors.Wrap(err, "deploy failed")
	}
	if err := d.WaitServiceStable(ctx, time.Now()); err != nil {
		return errors.Wrap(err, "deploy failed")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) Rollback(opt RollbackOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting rollback")
	service, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "rollback failed")
	}
	currentArn := *service.TaskDefinition
	targetArn, err := d.FindRollbackTarget(ctx, currentArn)
	if err != nil {
		return errors.Wrap(err, "rollback failed")
	}
	d.Log("Rollbacking to", arnToName(targetArn))
	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	if err := d.UpdateService(ctx, targetArn, *service.DesiredCount, false); err != nil {
		return errors.Wrap(err, "rollback failed")
	}
	if err := d.WaitServiceStable(ctx, time.Now()); err != nil {
		return errors.Wrap(err, "rollback failed")
	}

	d.Log("Service is stable now. Completed!")

	if *opt.DeregisterTaskDefinition {
		d.Log("Deregistering rolled back task definition", arnToName(currentArn))
		_, err := d.ecs.DeregisterTaskDefinitionWithContext(
			ctx,
			&ecs.DeregisterTaskDefinitionInput{
				TaskDefinition: &currentArn,
			},
		)
		if err != nil {
			return errors.Wrap(err, "deregister task definition failed")
		}
		d.Log(arnToName(currentArn), "was deregistered successfully")
	}

	return nil
}

func (d *App) FindRollbackTarget(ctx context.Context, taskDefinitionArn string) (string, error) {
	var found bool
	var nextToken *string
	family := strings.Split(arnToName(taskDefinitionArn), ":")[0]
	for {
		out, err := d.ecs.ListTaskDefinitionsWithContext(ctx,
			&ecs.ListTaskDefinitionsInput{
				NextToken:    nextToken,
				FamilyPrefix: aws.String(family),
				MaxResults:   aws.Int64(100),
				Sort:         aws.String("DESC"),
			},
		)
		if err != nil {
			return "", errors.Wrap(err, "list taskdefinitions failed")
		}
		if len(out.TaskDefinitionArns) == 0 {
			return "", errors.New("rollback target is not found")
		}
		nextToken = out.NextToken
		for _, tdArn := range out.TaskDefinitionArns {
			if found {
				return *tdArn, nil
			}
			if *tdArn == taskDefinitionArn {
				found = true
			}
		}
	}
}

func (d *App) Name() string {
	return fmt.Sprintf("%s/%s", d.Service, d.Cluster)
}

func (d *App) Log(v ...interface{}) {
	args := []interface{}{d.Name()}
	args = append(args, v...)
	log.Println(args...)
}

func (d *App) WaitServiceStable(ctx context.Context, startedAt time.Time) error {
	d.Log("Waiting for service stable...(it will take a few minutes)")
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		tick := time.Tick(10 * time.Second)
		var lines int
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-tick:
				if isTerminal {
					for i := 0; i < lines; i++ {
						fmt.Print(aec.EraseLine(aec.EraseModes.All), aec.PreviousLine(1))
					}
				}
				lines, _ = d.DescribeServiceDeployments(waitCtx, startedAt)
			}
		}
	}()

	return d.ecs.WaitUntilServicesStableWithContext(ctx, d.DescribeServicesInput())
}

func (d *App) UpdateService(ctx context.Context, taskDefinitionArn string, count int64, force bool) error {
	msg := "Updating service"
	if force {
		msg = msg + " with force new deployment"
	}
	msg = msg + "..."
	d.Log(msg)

	_, err := d.ecs.UpdateServiceWithContext(
		ctx,
		&ecs.UpdateServiceInput{
			Service:            aws.String(d.Service),
			Cluster:            aws.String(d.Cluster),
			TaskDefinition:     aws.String(taskDefinitionArn),
			DesiredCount:       &count,
			ForceNewDeployment: &force,
		},
	)
	return err
}

func (d *App) RegisterTaskDefinition(ctx context.Context, td *ecs.TaskDefinition) (*ecs.TaskDefinition, error) {
	d.Log("Registering a new task definition...")

	out, err := d.ecs.RegisterTaskDefinitionWithContext(
		ctx,
		&ecs.RegisterTaskDefinitionInput{
			ContainerDefinitions:    td.ContainerDefinitions,
			Cpu:                     td.Cpu,
			ExecutionRoleArn:        td.ExecutionRoleArn,
			Family:                  td.Family,
			Memory:                  td.Memory,
			NetworkMode:             td.NetworkMode,
			PlacementConstraints:    td.PlacementConstraints,
			RequiresCompatibilities: td.RequiresCompatibilities,
			TaskRoleArn:             td.TaskRoleArn,
			Volumes:                 td.Volumes,
		},
	)
	if err != nil {
		return nil, err
	}
	d.Log("Task definition is registered", taskDefinitionName(out.TaskDefinition))
	return out.TaskDefinition, nil
}

func (d *App) LoadTaskDefinition(path string) (*ecs.TaskDefinition, error) {
	d.Log("Creating a new task definition by", path)
	c := struct {
		TaskDefinition *ecs.TaskDefinition
	}{}
	if err := config.LoadWithEnvJSON(&c, path); err != nil {
		return nil, err
	}
	if c.TaskDefinition != nil {
		return c.TaskDefinition, nil
	}
	var td ecs.TaskDefinition
	if err := config.LoadWithEnvJSON(&td, path); err != nil {
		return nil, err
	}
	return &td, nil
}

func (d *App) LoadServiceDefinition(path string) (*ecs.CreateServiceInput, error) {
	c := ServiceDefinition{}
	if err := config.LoadWithEnvJSON(&c, path); err != nil {
		return nil, err
	}

	var count *int64
	if c.DesiredCount == nil {
		count = aws.Int64(1)
	} else {
		count = c.DesiredCount
	}

	return &ecs.CreateServiceInput{
		Cluster:                 aws.String(d.config.Cluster),
		DesiredCount:            count,
		ServiceName:             aws.String(d.config.Service),
		DeploymentConfiguration: c.DeploymentConfiguration,
		LaunchType:              c.LaunchType,
		LoadBalancers:           c.LoadBalancers,
		NetworkConfiguration:    c.NetworkConfiguration,
		PlacementConstraints:    c.PlacementConstraints,
		PlacementStrategy:       c.PlacementStrategy,
		Role:                    c.Role,
	}, nil
}

func NewTaskApp(conf *Config) (*App, error) {
	if err := conf.TaskValidate(); err != nil {
		return nil, errors.Wrap(err, "invalid configuration")
	}
	sess := session.Must(session.NewSession(
		&aws.Config{Region: aws.String(conf.Region)},
	))
	d := &App{
		ecs:    ecs.New(sess),
		config: conf,
	}
	return d, nil
}

func (d *App) TaskCreate(opt TaskCreateOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Starting create task-definition")

	td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
	if err != nil {
		return errors.Wrap(err, "create failed")
	}

	if *opt.DryRun {
		d.Log("task definition:", td.String())
		d.Log("DRY RUN OK")
		return nil
	}

	_, err = d.RegisterTaskDefinition(ctx, td)
	if err != nil {
		return errors.Wrap(err, "create failed")
	}

	d.Log("TaskDefinition is created")

	return nil
}
