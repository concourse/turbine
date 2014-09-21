package outputs

import (
	"fmt"

	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/concourse/turbine/api/builds"
	"github.com/concourse/turbine/event"
	"github.com/concourse/turbine/logwriter"
	"github.com/concourse/turbine/resource"
)

type Performer interface {
	PerformOutputs(warden.Container, []builds.Output, event.Emitter, <-chan struct{}) ([]builds.Output, error)
}

func NewParallelPerformer(tracker resource.Tracker) Performer {
	return parallelPerformer{tracker: tracker}
}

type parallelPerformer struct {
	tracker resource.Tracker
}

func (p parallelPerformer) PerformOutputs(
	container warden.Container,
	outputs []builds.Output,
	emitter event.Emitter,
	abort <-chan struct{},
) ([]builds.Output, error) {
	resultingOutputs := make([]builds.Output, len(outputs))

	errResults := make(chan error, len(outputs))

	for i, output := range outputs {
		go func(i int, output builds.Output) {
			streamOut, err := container.StreamOut("/tmp/build/src/")
			if err != nil {
				errResults <- err
				return
			}

			eventLog := logwriter.NewWriter(emitter, event.Origin{
				Type: event.OriginTypeOutput,
				Name: output.Name,
			})

			resource, err := p.tracker.Init(output.Type, eventLog, abort)
			if err != nil {
				errResults <- err
				return
			}

			defer p.tracker.Release(resource)

			computedOutput, err := resource.Out(streamOut, output)
			if err != nil {
				emitter.EmitEvent(event.Error{
					Message: fmt.Sprintf(output.Name+" output failed: %s", err),
				})

				errResults <- err
				return
			}

			emitter.EmitEvent(event.Output{Output: computedOutput})

			resultingOutputs[i] = computedOutput

			errResults <- nil
		}(i, output)
	}

	var outputErr error
	for i := 0; i < len(outputs); i++ {
		err := <-errResults
		if err != nil {
			outputErr = err
		}
	}

	if outputErr != nil {
		return nil, outputErr
	}

	return resultingOutputs, nil
}
