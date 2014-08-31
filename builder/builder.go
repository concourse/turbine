package builder

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloudfoundry-incubator/garden/warden"

	"github.com/concourse/turbine/api/builds"
	"github.com/concourse/turbine/event"
	"github.com/concourse/turbine/logwriter"
	"github.com/concourse/turbine/resource"
)

var ErrAborted = errors.New("build aborted")

type Builder interface {
	Start(builds.Build, <-chan struct{}) (RunningBuild, error)
	Attach(RunningBuild, <-chan struct{}) (SucceededBuild, error, error)
	Hijack(RunningBuild, warden.ProcessSpec, warden.ProcessIO) (warden.Process, error)
	Complete(SucceededBuild, <-chan struct{}) (builds.Build, error)
}

type RunningBuild struct {
	Build builds.Build

	ContainerHandle string
	Container       warden.Container

	ProcessID uint32
	Process   warden.Process

	Emitter event.Emitter
}

type SucceededBuild struct {
	Build builds.Build

	ContainerHandle string
	Container       warden.Container

	Emitter event.Emitter
}

type builder struct {
	tracker       resource.Tracker
	wardenClient  warden.Client
	createEmitter EmitterFactory
}

type EmitterFactory func(logsURL string) event.Emitter

func NewBuilder(
	tracker resource.Tracker,
	wardenClient warden.Client,
	createEmitter EmitterFactory,
) Builder {
	return &builder{
		tracker:       tracker,
		wardenClient:  wardenClient,
		createEmitter: createEmitter,
	}
}

type nullSink struct{}

func (nullSink) Write(data []byte) (int, error) { return len(data), nil }
func (nullSink) Close() error                   { return nil }

func (builder *builder) Start(build builds.Build, abort <-chan struct{}) (RunningBuild, error) {
	emitter := builder.createEmitter(build.EventsCallback)

	resources := map[string]io.Reader{}

	for i, input := range build.Inputs {
		eventLog := logwriter.NewWriter(emitter, event.Origin{
			Type: event.OriginTypeInput,
			Name: input.Name,
		})

		resource, err := builder.tracker.Init(input.Type, eventLog, abort)
		if err != nil {
			emitter.Close()
			return RunningBuild{}, err
		}

		defer builder.tracker.Release(resource)

		tarStream, computedInput, buildConfig, err := resource.In(input)
		if err != nil {
			emitter.Close()
			return RunningBuild{}, err
		}

		build.Inputs[i] = computedInput

		build.Config = build.Config.Merge(buildConfig)

		resources[input.Name] = tarStream
	}

	container, err := builder.createBuildContainer(build.Config, emitter)
	if err != nil {
		emitter.Close()
		return RunningBuild{}, err
	}

	err = builder.streamInResources(container, resources, build.Config.Paths)
	if err != nil {
		emitter.Close()
		return RunningBuild{}, err
	}

	process, err := builder.runBuild(container, build.Privileged, build.Config, emitter)
	if err != nil {
		emitter.Close()
		return RunningBuild{}, err
	}

	return RunningBuild{
		Build: build,

		ContainerHandle: container.Handle(),
		Container:       container,

		ProcessID: process.ID(),
		Process:   process,

		Emitter: emitter,
	}, nil
}

func (builder *builder) Attach(running RunningBuild, abort <-chan struct{}) (SucceededBuild, error, error) {
	if running.Emitter == nil {
		running.Emitter = builder.createEmitter(running.Build.EventsCallback)
	}

	if running.Container == nil {
		container, err := builder.wardenClient.Lookup(running.ContainerHandle)
		if err != nil {
			running.Emitter.Close()
			return SucceededBuild{}, nil, err
		}

		running.Container = container
	}

	if running.Process == nil {
		process, err := running.Container.Attach(
			running.ProcessID,
			emitterProcessIO(running.Emitter),
		)
		if err != nil {
			running.Emitter.Close()
			return SucceededBuild{}, nil, err
		}

		running.Process = process
	}

	status, err := builder.waitForRunToEnd(running, abort)
	if err != nil {
		running.Emitter.Close()
		return SucceededBuild{}, nil, err
	}

	if status != 0 {
		return SucceededBuild{}, fmt.Errorf("exit status %d", status), nil
	}

	return SucceededBuild{
		Build:     running.Build,
		Container: running.Container,

		Emitter: running.Emitter,
	}, nil, nil
}

func (builder *builder) Complete(succeeded SucceededBuild, abort <-chan struct{}) (builds.Build, error) {
	if succeeded.Emitter == nil {
		succeeded.Emitter = builder.createEmitter(succeeded.Build.EventsCallback)
	}

	defer succeeded.Emitter.Close()

	outputs, err := builder.performOutputs(succeeded.Container, succeeded.Build, succeeded.Emitter, abort)
	if err != nil {
		return builds.Build{}, err
	}

	succeeded.Build.Outputs = outputs

	return succeeded.Build, nil
}

func (builder *builder) Hijack(running RunningBuild, spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
	container, err := builder.wardenClient.Lookup(running.ContainerHandle)
	if err != nil {
		return nil, err
	}

	return container.Run(spec, io)
}

func (builder *builder) createBuildContainer(
	buildConfig builds.Config,
	emitter event.Emitter,
) (warden.Container, error) {
	emitter.EmitEvent(event.Initialize{
		BuildConfig: buildConfig,
	})

	containerSpec := warden.ContainerSpec{
		RootFSPath: buildConfig.Image,
	}

	return builder.wardenClient.Create(containerSpec)
}

func (builder *builder) streamInResources(
	container warden.Container,
	resources map[string]io.Reader,
	paths map[string]string,
) error {
	for name, streamOut := range resources {
		destination, found := paths[name]
		if !found {
			destination = name
		}

		err := container.StreamIn("/tmp/build/src/"+destination, streamOut)
		if err != nil {
			return err
		}
	}

	return nil
}

func (builder *builder) runBuild(
	container warden.Container,
	privileged bool,
	buildConfig builds.Config,
	emitter event.Emitter,
) (warden.Process, error) {
	emitter.EmitEvent(event.Start{
		Time: time.Now().Unix(),
	})

	env := []string{}
	for n, v := range buildConfig.Params {
		env = append(env, n+"="+v)
	}

	return container.Run(warden.ProcessSpec{
		Path: buildConfig.Run.Path,
		Args: buildConfig.Run.Args,
		Env:  env,
		Dir:  "/tmp/build/src",

		TTY: &warden.TTYSpec{},

		Privileged: privileged,
	}, emitterProcessIO(emitter))
}

func (builder *builder) waitForRunToEnd(
	running RunningBuild,
	abort <-chan struct{},
) (int, error) {
	statusCh := make(chan int, 1)
	errCh := make(chan error, 1)

	go func() {
		status, err := running.Process.Wait()
		if err != nil {
			errCh <- err
		} else {
			statusCh <- status
		}
	}()

	select {
	case status := <-statusCh:
		return status, nil

	case err := <-errCh:
		return 0, err

	case <-abort:
		running.Container.Stop(false)
		return 0, ErrAborted
	}
}

func (builder *builder) performOutputs(
	container warden.Container,
	build builds.Build,
	emitter event.Emitter,
	abort <-chan struct{},
) ([]builds.Output, error) {
	allOutputs := map[string]builds.Output{}

	// implicit outputs
	for _, input := range build.Inputs {
		allOutputs[input.Name] = builds.Output{
			Name:     input.Name,
			Type:     input.Type,
			Source:   input.Source,
			Version:  input.Version,
			Metadata: input.Metadata,
		}
	}

	if len(build.Outputs) > 0 {
		errs := make(chan error, len(build.Outputs))
		results := make(chan builds.Output, len(build.Outputs))

		for _, output := range build.Outputs {
			go func(output builds.Output) {
				inputOutput, found := allOutputs[output.Name]
				if found {
					output.Version = inputOutput.Version
				}

				streamOut, err := container.StreamOut("/tmp/build/src/")
				if err != nil {
					errs <- err
					return
				}

				eventLog := logwriter.NewWriter(emitter, event.Origin{
					Type: event.OriginTypeOutput,
					Name: output.Name,
				})

				resource, err := builder.tracker.Init(output.Type, eventLog, abort)
				if err != nil {
					errs <- err
					return
				}

				defer builder.tracker.Release(resource)

				computedOutput, err := resource.Out(streamOut, output)

				errs <- err
				results <- computedOutput
			}(output)
		}

		var outputErr error
		for i := 0; i < len(build.Outputs); i++ {
			err := <-errs
			if err != nil {
				outputErr = err
			}
		}

		if outputErr != nil {
			return nil, outputErr
		}

		for i := 0; i < len(build.Outputs); i++ {
			output := <-results
			allOutputs[output.Name] = output
		}
	}

	outputs := []builds.Output{}
	for _, output := range allOutputs {
		outputs = append(outputs, output)
	}

	return outputs, nil
}

func emitterProcessIO(emitter event.Emitter) warden.ProcessIO {
	return warden.ProcessIO{
		Stdout: logwriter.NewWriter(emitter, event.Origin{
			Type: event.OriginTypeRun,
			Name: "stdout",
		}),
		Stderr: logwriter.NewWriter(emitter, event.Origin{
			Type: event.OriginTypeRun,
			Name: "stderr",
		}),
	}
}
