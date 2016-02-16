package scheduler

import (
	"sync"
	"time"

	"github.com/pivotal-golang/lager"

	"github.com/concourse/atc"
	"github.com/concourse/atc/config"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/db/algorithm"
	"github.com/concourse/atc/engine"
)

//go:generate counterfeiter . PipelineDB

type PipelineDB interface {
	CreateJobBuild(job string) (db.Build, error)
	CreateJobBuildForCandidateInputs(job string) (db.Build, bool, error)
	ScheduleBuild(buildID int, jobConfig atc.JobConfig) (bool, error)

	GetJobBuildForInputs(job string, inputs []db.BuildInput) (db.Build, bool, error)
	GetNextPendingBuild(job string) (db.Build, bool, error)

	LoadVersionsDB() (*algorithm.VersionsDB, error)
	GetLatestInputVersions(versions *algorithm.VersionsDB, job string, inputs []config.JobInput) ([]db.BuildInput, bool, error)
	SaveResourceVersions(atc.ResourceConfig, []atc.Version) error
	UseInputsForBuild(buildID int, inputs []db.BuildInput) error
}

//go:generate counterfeiter . BuildsDB

type BuildsDB interface {
	LeaseBuildScheduling(buildID int, interval time.Duration) (db.Lease, bool, error)
	GetAllStartedBuilds() ([]db.Build, error)
	ErrorBuild(buildID int, err error) error
	FinishBuild(int, db.Status) error

	GetBuildPreparation(buildID int) (db.BuildPreparation, bool, error)
	UpdateBuildPreparation(buildPreparation db.BuildPreparation) error
}

//go:generate counterfeiter . BuildFactory

type BuildFactory interface {
	Create(atc.JobConfig, atc.ResourceConfigs, []db.BuildInput) (atc.Plan, error)
}

type Waiter interface {
	Wait()
}

//go:generate counterfeiter . Scanner

type Scanner interface {
	Scan(lager.Logger, string) error
}

type Scheduler struct {
	PipelineDB PipelineDB
	BuildsDB   BuildsDB
	Factory    BuildFactory
	Engine     engine.Engine
	Scanner    Scanner
}

func (s *Scheduler) BuildLatestInputs(logger lager.Logger, versions *algorithm.VersionsDB, job atc.JobConfig, resources atc.ResourceConfigs) error {
	logger = logger.Session("build-latest")

	inputs := config.JobInputs(job)

	if len(inputs) == 0 {
		// no inputs; no-op
		return nil
	}

	latestInputs, found, err := s.PipelineDB.GetLatestInputVersions(versions, job.Name, inputs)
	if err != nil {
		logger.Error("failed-to-get-latest-input-versions", err)
		return err
	}

	if !found {
		logger.Debug("no-input-versions-available")
		return nil
	}

	checkInputs := []db.BuildInput{}
	for _, input := range latestInputs {
		for _, ji := range inputs {
			if ji.Name == input.Name {
				if ji.Trigger {
					checkInputs = append(checkInputs, input)
				}

				break
			}
		}
	}

	if len(checkInputs) == 0 {
		logger.Debug("no-triggered-input-versions")
		return nil
	}

	existingBuild, found, err := s.PipelineDB.GetJobBuildForInputs(job.Name, checkInputs)
	if err != nil {
		logger.Error("could-not-determine-if-inputs-are-already-used", err)
		return err
	}

	if found {
		logger.Debug("build-already-exists-for-inputs", lager.Data{
			"existing-build": existingBuild.ID,
		})

		return nil
	}

	build, created, err := s.PipelineDB.CreateJobBuildForCandidateInputs(job.Name)
	if err != nil {
		logger.Error("failed-to-create-build", err)
		return err
	}

	if !created {
		logger.Debug("waiting-for-existing-build-to-determine-inputs", lager.Data{
			"existing-build": build.ID,
		})
		return nil
	}

	logger.Debug("created-build", lager.Data{"build": build.ID})

	// NOTE: this is intentionally serial within a scheduler tick, so that
	// multiple ATCs don't do redundant work to determine a build's inputs.

	s.scheduleAndResumePendingBuild(logger, versions, build, job, resources)

	return nil
}

func (s *Scheduler) TryNextPendingBuild(logger lager.Logger, versions *algorithm.VersionsDB, job atc.JobConfig, resources atc.ResourceConfigs) Waiter {
	logger = logger.Session("try-next-pending")

	wg := new(sync.WaitGroup)

	wg.Add(1)
	go func() {
		defer wg.Done()

		build, found, err := s.PipelineDB.GetNextPendingBuild(job.Name)
		if err != nil {
			logger.Error("failed-to-get-next-pending-build", err)
			return
		}

		if !found {
			return
		}

		s.scheduleAndResumePendingBuild(logger, versions, build, job, resources)
	}()

	return wg
}

func (s *Scheduler) TriggerImmediately(logger lager.Logger, job atc.JobConfig, resources atc.ResourceConfigs) (db.Build, Waiter, error) {
	logger = logger.Session("trigger-immediately")

	build, err := s.PipelineDB.CreateJobBuild(job.Name)
	if err != nil {
		logger.Error("failed-to-create-build", err)
		return db.Build{}, nil, err
	}

	wg := new(sync.WaitGroup)
	wg.Add(1)

	// do not block request on scanning input versions
	go func() {
		defer wg.Done()
		s.scheduleAndResumePendingBuild(logger, nil, build, job, resources)
	}()

	return build, wg, nil
}

func (s *Scheduler) scheduleAndResumePendingBuild(logger lager.Logger, versions *algorithm.VersionsDB, build db.Build, job atc.JobConfig, resources atc.ResourceConfigs) engine.Build {
	lease, acquired, err := s.BuildsDB.LeaseBuildScheduling(build.ID, 10*time.Second)
	if err != nil {
		logger.Error("failed-to-get-lease", err)
		return nil
	}

	if !acquired {
		return nil
	}

	defer lease.Break()

	logger = logger.WithData(lager.Data{"build": build.ID})

	scheduled, err := s.PipelineDB.ScheduleBuild(build.ID, job)
	if err != nil {
		logger.Error("failed-to-schedule-build", err)
		return nil
	}

	if !scheduled {
		logger.Debug("build-could-not-be-scheduled")
		return nil
	}

	buildInputs := config.JobInputs(job)
	buildPrep, found, err := s.BuildsDB.GetBuildPreparation(build.ID)
	if err != nil {
		logger.Error("failed-to-get-build-prep", err, lager.Data{"build-id": build.ID})
		return nil
	}

	if !found {
		logger.Debug("failed-to-find-build-prep", lager.Data{"build-id": build.ID})
		return nil
	}

	if versions == nil {
		for _, input := range buildInputs {
			buildPrep.Inputs[input.Name] = db.BuildPreparationStatusUnknown
		}

		buildPrep.InputsSatisfied = db.BuildPreparationStatusBlocking

		err = s.BuildsDB.UpdateBuildPreparation(buildPrep)
		if err != nil {
			logger.Error("failed-to-update-build-prep-with-inputs", err, lager.Data{"build-id": build.ID})
			return nil
		}

		for _, input := range buildInputs {
			scanLog := logger.Session("scan", lager.Data{
				"input":    input.Name,
				"resource": input.Resource,
			})

			buildPrep = s.cloneBuildPrep(buildPrep)
			buildPrep.Inputs[input.Name] = db.BuildPreparationStatusBlocking
			err := s.BuildsDB.UpdateBuildPreparation(buildPrep)
			if err != nil {
				logger.Error("failed-to-update-build-prep-with-blocking-input", err, lager.Data{"build-id": build.ID})
				return nil
			}

			err = s.Scanner.Scan(scanLog, input.Resource)
			if err != nil {
				scanLog.Error("failed-to-scan", err)

				err := s.BuildsDB.ErrorBuild(build.ID, err)
				if err != nil {
					logger.Error("failed-to-mark-build-as-errored", err)
				}

				return nil
			}

			buildPrep = s.cloneBuildPrep(buildPrep)
			buildPrep.Inputs[input.Name] = db.BuildPreparationStatusNotBlocking
			err = s.BuildsDB.UpdateBuildPreparation(buildPrep)
			if err != nil {
				logger.Error("failed-to-update-build-prep-with-not-blocking-input", err, lager.Data{"build-id": build.ID})
				return nil
			}

			scanLog.Info("done")
		}

		loadStart := time.Now()

		vLog := logger.Session("loading-versions")
		vLog.Info("start")

		versions, err = s.PipelineDB.LoadVersionsDB()
		if err != nil {
			vLog.Error("failed", err)
			return nil
		}

		vLog.Info("done", lager.Data{"took": time.Since(loadStart).String()})
	} else {
		for _, input := range buildInputs {
			buildPrep.Inputs[input.Name] = db.BuildPreparationStatusNotBlocking
		}
		buildPrep.InputsSatisfied = db.BuildPreparationStatusBlocking
		err := s.BuildsDB.UpdateBuildPreparation(buildPrep)
		if err != nil {
			logger.Error("failed-to-update-build-prep-with-discovered-inputs", err)
			return nil
		}
	}

	inputs, found, err := s.PipelineDB.GetLatestInputVersions(versions, job.Name, buildInputs)
	if err != nil {
		logger.Error("failed-to-get-latest-input-versions", err)
		return nil
	}

	if !found {
		logger.Debug("no-input-versions-available")
		return nil
	}

	buildPrep.InputsSatisfied = db.BuildPreparationStatusNotBlocking
	err = s.BuildsDB.UpdateBuildPreparation(buildPrep)
	if err != nil {
		logger.Error("failed-to-update-build-prep-with-inputs-satisfied", err)
		return nil
	}

	err = s.PipelineDB.UseInputsForBuild(build.ID, inputs)
	if err != nil {
		logger.Error("failed-to-use-inputs-for-build", err)
		return nil
	}

	plan, err := s.Factory.Create(job, resources, inputs)
	if err != nil {
		// Don't use ErrorBuild because it logs a build event, and this build hasn't started
		err := s.BuildsDB.FinishBuild(build.ID, db.StatusErrored)
		if err != nil {
			logger.Error("failed-to-mark-build-as-errored", err)
		}
		return nil
	}

	createdBuild, err := s.Engine.CreateBuild(logger, build, plan)
	if err != nil {
		logger.Error("failed-to-create-build", err)
		return nil
	}

	if createdBuild != nil {
		logger.Info("building")
		go createdBuild.Resume(logger)
	}

	return createdBuild
}

// Turns out that counterfieter clones the pointer in the build prep so when
// the build prep gets modified, so does the copy in the fake. This clone is
// done to get around this. God damn it counterfeiter.
func (s *Scheduler) cloneBuildPrep(buildPrep db.BuildPreparation) db.BuildPreparation {
	clone := db.BuildPreparation{
		BuildID:          buildPrep.BuildID,
		PausedPipeline:   buildPrep.PausedPipeline,
		PausedJob:        buildPrep.PausedJob,
		MaxRunningBuilds: buildPrep.MaxRunningBuilds,
		Inputs:           map[string]db.BuildPreparationStatus{},
		InputsSatisfied:  buildPrep.InputsSatisfied,
	}

	for key, value := range buildPrep.Inputs {
		clone.Inputs[key] = value
	}

	return clone
}
