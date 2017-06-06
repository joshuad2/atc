package exec

import (
	"os"

	"code.cloudfoundry.org/lager"

	"github.com/concourse/atc/resource"
	"github.com/concourse/atc/worker"
)

type RootFSSource struct {
	Base   *BaseResourceTypeRootFSSource
	Output *OutputRootFSSource
}

type BaseResourceTypeRootFSSource struct {
	Name string
}

type OutputRootFSSource struct {
	Name string
}

type Action interface {
	Run(lager.Logger, *worker.ArtifactRepository, <-chan os.Signal, chan<- struct{}) error
}

func newActionsStep(
	logger lager.Logger, // TODO: can we move that to method? need to change all steps though
	actions []Action,
	buildEventsDelegate BuildEventsDelegate,
) ActionsStep {
	return ActionsStep{
		logger:              logger,
		actions:             actions,
		buildEventsDelegate: buildEventsDelegate,
	}
}

type BuildEventsDelegate interface {
	Initializing(lager.Logger)

	Failed(lager.Logger, error)
	Finished(lager.Logger, ExitStatus)
}

type ActionsStep struct {
	actions             []Action
	buildEventsDelegate BuildEventsDelegate

	logger lager.Logger // TODO: can we move that to method? need to change all steps though

	repository *worker.ArtifactRepository
	succeeded  bool
}

func (s ActionsStep) Using(prev Step, repo *worker.ArtifactRepository) Step {
	s.repository = repo
	return &s
}

func (s *ActionsStep) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	s.buildEventsDelegate.Initializing(s.logger)

	for _, action := range s.actions {
		err := action.Run(s.logger, s.repository, signals, ready)
		if err != nil {
			if err, ok := err.(resource.ErrResourceScriptFailed); ok {
				s.logger.Error("resource-script-failed", err)
				s.buildEventsDelegate.Finished(s.logger, ExitStatus(err.ExitStatus))
				return nil
			}

			if err == resource.ErrAborted {
				s.logger.Debug("resource-aborted")
				return ErrInterrupted
			}

			s.logger.Error("failed-to-run-action", err)
			s.buildEventsDelegate.Failed(s.logger, err)
			return err
		}
	}

	s.buildEventsDelegate.Finished(s.logger, ExitStatus(0))

	s.succeeded = true

	return nil
}

func (s *ActionsStep) Result(x interface{}) bool {
	switch v := x.(type) {
	case *Success:
		*v = Success(s.succeeded)
		return true

	default:
		return false
	}
}