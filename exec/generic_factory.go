package exec

import (
	"crypto/sha1"
	"fmt"
	"path/filepath"

	"code.cloudfoundry.org/lager"

	"github.com/concourse/atc"
	"github.com/concourse/atc/creds"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/resource"
	"github.com/concourse/atc/runtime"
	"github.com/concourse/atc/worker"
)

type factory struct {
	orchestrator           runtime.Orchestrator
	resourceFetcher        resource.Fetcher
	resourceFactory        resource.ResourceFactory
	dbResourceCacheFactory db.ResourceCacheFactory
	variablesFactory       creds.VariablesFactory
}

func NewFactory(
	orchestrator runtime.Orchestrator,
	resourceFetcher resource.Fetcher,
	resourceFactory resource.ResourceFactory,
	dbResourceCacheFactory db.ResourceCacheFactory,
	variablesFactory creds.VariablesFactory,
) Factory {
	return &factory{
		orchestrator:           orchestrator,
		resourceFetcher:        resourceFetcher,
		resourceFactory:        resourceFactory,
		dbResourceCacheFactory: dbResourceCacheFactory,
		variablesFactory:       variablesFactory,
	}
}

func (factory *factory) Get(
	logger lager.Logger,
	plan atc.Plan,
	build db.Build,
	stepMetadata StepMetadata,
	workerMetadata db.ContainerMetadata,
	delegate GetDelegate,
) Step {
	workerMetadata.WorkingDirectory = resource.ResourcesDir("get")

	variables := factory.variablesFactory.NewVariables(build.TeamName(), build.PipelineName())

	getStep := NewGetStep(
		build,

		plan.Get.Name,
		plan.Get.Type,
		plan.Get.Resource,
		creds.NewSource(variables, plan.Get.Source),
		creds.NewParams(variables, plan.Get.Params),
		NewVersionSourceFromPlan(plan.Get),
		plan.Get.Tags,

		delegate,
		factory.resourceFetcher,
		build.TeamID(),
		build.ID(),
		plan.ID,
		workerMetadata,
		factory.dbResourceCacheFactory,
		stepMetadata,

		creds.NewVersionedResourceTypes(variables, plan.Get.VersionedResourceTypes),
	)

	return LogError(getStep, delegate)
}

func (factory *factory) Put(
	logger lager.Logger,
	plan atc.Plan,
	build db.Build,
	stepMetadata StepMetadata,
	workerMetadata db.ContainerMetadata,
	delegate PutDelegate,
) Step {
	workerMetadata.WorkingDirectory = resource.ResourcesDir("put")

	variables := factory.variablesFactory.NewVariables(build.TeamName(), build.PipelineName())

	putStep := NewPutStep(
		build,

		plan.Put.Name,
		plan.Put.Type,
		plan.Put.Resource,
		creds.NewSource(variables, plan.Put.Source),
		creds.NewParams(variables, plan.Put.Params),
		plan.Put.Tags,

		delegate,
		factory.resourceFactory,
		plan.ID,
		workerMetadata,
		stepMetadata,

		creds.NewVersionedResourceTypes(variables, plan.Put.VersionedResourceTypes),
	)

	return LogError(putStep, delegate)
}

func (factory *factory) Task(
	logger lager.Logger,
	plan atc.Plan,
	build db.Build,
	containerMetadata db.ContainerMetadata,
	delegate TaskDelegate,
) Step {
	workingDirectory := factory.taskWorkingDirectory(worker.ArtifactName(plan.Task.Name))
	containerMetadata.WorkingDirectory = workingDirectory

	var taskConfigSource TaskConfigSource
	if plan.Task.ConfigPath != "" && (plan.Task.Config != nil || plan.Task.Params != nil) {
		taskConfigSource = MergedConfigSource{
			A: FileConfigSource{plan.Task.ConfigPath},
			B: StaticConfigSource{Plan: *plan.Task},
		}
	} else if plan.Task.Config != nil {
		taskConfigSource = StaticConfigSource{Plan: *plan.Task}
	} else if plan.Task.ConfigPath != "" {
		taskConfigSource = FileConfigSource{plan.Task.ConfigPath}
	}

	taskConfigSource = ValidatingConfigSource{ConfigSource: taskConfigSource}

	taskConfigSource = DeprecationConfigSource{
		Delegate: taskConfigSource,
		Stderr:   delegate.Stderr(),
	}

	variables := factory.variablesFactory.NewVariables(build.TeamName(), build.PipelineName())

	taskStep := NewTaskStep(
		Privileged(plan.Task.Privileged),
		taskConfigSource,
		plan.Task.Tags,
		plan.Task.InputMapping,
		plan.Task.OutputMapping,

		workingDirectory,
		plan.Task.ImageArtifactName,

		delegate,

		factory.orchestrator,
		build.TeamID(),
		build.ID(),
		build.JobID(),
		plan.Task.Name,
		plan.ID,
		containerMetadata,

		creds.NewVersionedResourceTypes(variables, plan.Task.VersionedResourceTypes),
		variables,
	)

	return LogError(taskStep, delegate)
}

func (factory *factory) taskWorkingDirectory(sourceName worker.ArtifactName) string {
	sum := sha1.Sum([]byte(sourceName))
	return filepath.Join("/tmp", "build", fmt.Sprintf("%x", sum[:4]))
}
