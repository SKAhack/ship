package cmd

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"

	"github.com/docker/distribution/reference"
	"github.com/oklog/ulid"
	"github.com/spf13/cobra"

	libecs "github.com/SKAhack/shipctl/lib/ecs"
	log "github.com/SKAhack/shipctl/lib/logger"
)

var ECRRegex *regexp.Regexp = func() *regexp.Regexp {
	regex, _ := regexp.Compile(`^[0-9]+\.dkr\.ecr\.(us|ca|eu|ap|sa)-(east|west|central|northeast|southeast|south)-[12]\.amazonaws\.com$`)
	return regex
}()

type deployCmd struct {
	cluster         string
	serviceName     string
	revision        int
	images          imageOptions
	backend         string
	slackWebhookUrl string
}

func NewDeployCommand(out, errOut io.Writer) *cobra.Command {
	f := &deployCmd{}
	cmd := &cobra.Command{
		Use:   "deploy [options]",
		Short: "",
		RunE: func(cmd *cobra.Command, args []string) error {
			l := log.NewLogger(f.cluster, f.serviceName, f.slackWebhookUrl, out)
			err := f.execute(cmd, args, l)
			if err != nil {
				msg := fmt.Sprintf("failed to deploy. cluster: %s, serviceName: %s\n", f.cluster, f.serviceName)
				l.Log(msg)
				l.Slack("danger", msg)
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.cluster, "cluster", "", "ECS Cluster Name")
	cmd.Flags().StringVar(&f.serviceName, "service-name", "", "ECS Service Name")
	cmd.Flags().IntVar(&f.revision, "revision", 0, "revision of ECS task definition")
	cmd.Flags().Var(&f.images, "image", "base image of ECR image")
	cmd.Flags().StringVar(&f.backend, "backend", "SSM", "Backend type of history manager")
	cmd.Flags().StringVar(&f.slackWebhookUrl, "slack-webhook-url", "", "slack webhook URL")

	return cmd
}

func (f *deployCmd) execute(_ *cobra.Command, args []string, l *log.Logger) error {
	if f.cluster == "" {
		return errors.New("--cluster is required")
	}

	if f.serviceName == "" {
		return errors.New("--service-name is required")
	}

	if len(f.images.Value) == 0 {
		return errors.New("--image is required")
	}

	region := getAWSRegion()
	if region == "" {
		return errors.New("AWS region is not found. please set a AWS_DEFAULT_REGION or AWS_REGION")
	}

	sess, err := session.NewSession()
	if err != nil {
		return err
	}

	client := ecs.New(sess, &aws.Config{
		Region: aws.String(region),
	})

	ecrClient := ecr.New(sess, &aws.Config{
		Region: aws.String(region),
	})

	historyManager, err := NewHistoryManager(f.backend, f.cluster, f.serviceName)
	if err != nil {
		return err
	}

	service, err := libecs.DescribeService(client, f.cluster, f.serviceName)
	if err != nil {
		return err
	}

	if len(service.Deployments) > 1 {
		return errors.New(fmt.Sprintf("%s is currently deploying", f.serviceName))
	}

	var uniqueID string
	{
		entropy := rand.New(rand.NewSource(time.Now().UnixNano()))
		uniqueID = ulid.MustNew(ulid.Now(), entropy).String()
	}

	var taskDef *ecs.TaskDefinition
	var registerdTaskDef *ecs.TaskDefinition
	{
		taskDefArn := *service.TaskDefinition
		taskDefArn, err = libecs.SpecifyRevision(f.revision, taskDefArn)
		if err != nil {
			return err
		}

		taskDef, err = libecs.DescribeTaskDefinition(client, taskDefArn)
		if err != nil {
			return err
		}

		newTaskDef, err := f.createNewTaskDefinition(uniqueID, taskDef)
		if err != nil {
			return err
		}

		for _, v := range taskDef.ContainerDefinitions {
			img, err := f.parseDockerImage(*v.Image)
			if err != nil {
				return err
			}

			opt := f.images.Get(img.RepositoryName)
			if opt == nil {
				return errors.New(fmt.Sprintf("can not found image option %s", img.RepositoryName))
			}

			err = f.tagDockerImage(ecrClient, img.RepositoryName, opt.Tag, uniqueID)
			if err != nil {
				return err
			}
		}

		registerdTaskDef, err = f.registerTaskDefinition(client, newTaskDef)
		if err != nil {
			return err
		}
	}

	var msg string
	msg = fmt.Sprintf("deploy: revision %d -> %d\n", *taskDef.Revision, *registerdTaskDef.Revision)
	l.Log(msg)
	l.Slack("normal", msg)

	err = libecs.UpdateService(client, service, registerdTaskDef)
	if err != nil {
		return err
	}

	l.Log(fmt.Sprintf("service updating\n"))

	err = libecs.WaitUpdateService(client, f.cluster, f.serviceName, l)
	if err != nil {
		return err
	}

	err = historyManager.PushState(
		int(*registerdTaskDef.Revision),
		fmt.Sprintf("deploy: %d -> %d", *taskDef.Revision, *registerdTaskDef.Revision),
	)
	if err != nil {
		return err
	}

	msg = fmt.Sprintf("successfully updated\n")
	l.Log(msg)
	l.Slack("good", msg)

	return nil
}

func (f *deployCmd) createNewTaskDefinition(id string, taskDef *ecs.TaskDefinition) (*ecs.TaskDefinition, error) {
	newTaskDef := *taskDef // shallow copy
	var containers []*ecs.ContainerDefinition
	for _, vp := range taskDef.ContainerDefinitions {
		v := *vp // shallow copy
		img, err := f.parseDockerImage(*v.Image)
		if err != nil {
			return nil, err
		}

		if f.isECRHosted(img) {
			v.Image = aws.String(fmt.Sprintf("%s:%s", img.Name, id))
			containers = append(containers, &v)
		}
	}
	newTaskDef.ContainerDefinitions = containers

	return &newTaskDef, nil
}

type dockerImage struct {
	Name           string
	Tag            string
	RepositoryName string
	HostName       string
}

func (f *deployCmd) parseDockerImage(image string) (*dockerImage, error) {
	ref, err := reference.Parse(image)
	if err != nil {
		return nil, err
	}

	hostName, repoName := reference.SplitHostname(ref.(reference.Named))
	return &dockerImage{
		Name:           ref.(reference.Named).Name(),
		Tag:            ref.(reference.Tagged).Tag(),
		RepositoryName: repoName,
		HostName:       hostName,
	}, nil
}

func (f *deployCmd) isECRHosted(image *dockerImage) bool {
	return ECRRegex.MatchString(image.HostName)
}

func (f *deployCmd) registerTaskDefinition(client *ecs.ECS, taskDef *ecs.TaskDefinition) (*ecs.TaskDefinition, error) {
	params := &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:    taskDef.ContainerDefinitions,
		Cpu:                     taskDef.Cpu,
		ExecutionRoleArn:        taskDef.ExecutionRoleArn,
		Family:                  taskDef.Family,
		Memory:                  taskDef.Memory,
		NetworkMode:             taskDef.NetworkMode,
		PlacementConstraints:    taskDef.PlacementConstraints,
		TaskRoleArn:             taskDef.TaskRoleArn,
		Volumes:                 taskDef.Volumes,
		RequiresCompatibilities: taskDef.RequiresCompatibilities,
	}

	res, err := client.RegisterTaskDefinition(params)
	if err != nil {
		return nil, err
	}

	return res.TaskDefinition, nil
}

func (f *deployCmd) tagDockerImage(ecrClient *ecr.ECR, repoName string, fromTag string, toTag string) error {
	params := &ecr.BatchGetImageInput{
		ImageIds:       []*ecr.ImageIdentifier{{ImageTag: aws.String(fromTag)}},
		RepositoryName: aws.String(repoName),

		AcceptedMediaTypes: []*string{
			aws.String("application/vnd.docker.distribution.manifest.v1+json"),
			aws.String("application/vnd.docker.distribution.manifest.v2+json"),
			aws.String("application/vnd.oci.image.manifest.v1+json"),
		},
	}
	img, err := ecrClient.BatchGetImage(params)
	if err != nil {
		return err
	}

	putParams := &ecr.PutImageInput{
		ImageManifest:  img.Images[0].ImageManifest,
		RepositoryName: aws.String(repoName),
		ImageTag:       aws.String(toTag),
	}
	_, err = ecrClient.PutImage(putParams)
	if err != nil {
		return err
	}

	return nil
}

//
// imageOptions
//

type imageOption struct {
	RepositoryName string
	Tag            string
}

type imageOptions struct {
	Value []*imageOption
}

func (t *imageOptions) String() string {
	return fmt.Sprintf("String: %v", t.Value)
}

func (t *imageOptions) Set(v string) error {
	r, _ := regexp.Compile(`^([a-z0-9]+(?:(?:[._]|__|[-]*)[a-z0-9]+)*):([\w][\w.-]{0,127})$`)
	matches := r.FindStringSubmatch(v)
	if len(matches) == 0 {
		return errors.New(fmt.Sprintf("invalid format %s", v))
	}

	opt := &imageOption{
		RepositoryName: matches[1],
		Tag:            matches[2],
	}

	t.Value = append(t.Value, opt)

	return nil
}

func (t *imageOptions) Type() string {
	return "image"
}

func (t *imageOptions) Get(repoName string) *imageOption {
	for _, v := range t.Value {
		if v.RepositoryName == repoName {
			return v
		}
	}
	return nil
}
