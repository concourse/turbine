package builder_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/cloudfoundry-incubator/garden/client/fake_warden_client"
	"github.com/cloudfoundry-incubator/garden/warden"
	wfakes "github.com/cloudfoundry-incubator/garden/warden/fakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/concourse/turbine/api/builds"
	. "github.com/concourse/turbine/builder"
	"github.com/concourse/turbine/builder/inputs"
	ifakes "github.com/concourse/turbine/builder/inputs/fakes"
	ofakes "github.com/concourse/turbine/builder/outputs/fakes"
	"github.com/concourse/turbine/event"
	efakes "github.com/concourse/turbine/event/fakes"
	"github.com/concourse/turbine/event/testlog"
	"github.com/concourse/turbine/resource"
)

var _ = Describe("Builder", func() {
	var (
		wardenClient    *fake_warden_client.FakeClient
		inputFetcher    *ifakes.FakeFetcher
		outputPerformer *ofakes.FakePerformer

		emitter *efakes.FakeEmitter
		events  *testlog.EventLog

		builder Builder

		build builds.Build
	)

	BeforeEach(func() {
		wardenClient = fake_warden_client.New()

		emitter = new(efakes.FakeEmitter)

		events = &testlog.EventLog{}
		emitter.EmitEventStub = events.Add

		inputFetcher = new(ifakes.FakeFetcher)
		outputPerformer = new(ofakes.FakePerformer)

		builder = NewBuilder(wardenClient, inputFetcher, outputPerformer)

		build = builds.Build{
			EventsCallback: "some-events-callback",

			Config: builds.Config{
				Image: "some-rootfs",

				Params: map[string]string{
					"FOO": "bar",
					"BAZ": "buzz",
				},

				Run: builds.RunConfig{
					Path: "./bin/test",
					Args: []string{"arg1", "arg2"},
				},
			},
		}
	})

	Describe("Start", func() {
		var (
			started  RunningBuild
			startErr error
		)

		BeforeEach(func() {
			build.Inputs = []builds.Input{
				{
					Name: "first-resource",
					Type: "raw",
				},
				{
					Name: "second-resource",
					Type: "raw",
				},
			}

			wardenClient.Connection.CreateReturns("some-handle", nil)

			runningProcess := new(wfakes.FakeProcess)
			runningProcess.IDReturns(42)

			wardenClient.Connection.RunReturns(runningProcess, nil)
		})

		var abort chan struct{}

		JustBeforeEach(func() {
			abort = make(chan struct{})
			started, startErr = builder.Start(build, emitter, abort)
		})

		Context("when fetching the build's inputs succeeds", func() {
			var (
				fetchedInputs []inputs.FetchedInput

				firstReleased  chan struct{}
				secondReleased chan struct{}
			)

			BeforeEach(func() {
				firstReleased = make(chan struct{})
				secondReleased = make(chan struct{})

				fetchedInputs = []inputs.FetchedInput{
					{
						Input: builds.Input{
							Name:     "first-resource",
							Type:     "raw",
							Version:  builds.Version{"version": "1"},
							Metadata: []builds.MetadataField{{Name: "key", Value: "meta-1"}},
						},
						Stream: bytes.NewBufferString("some-data-1"),
						Release: func() error {
							close(firstReleased)
							return nil
						},
					},
					{
						Input: builds.Input{
							Name:     "second-resource",
							Type:     "raw",
							Version:  builds.Version{"version": "2"},
							Metadata: []builds.MetadataField{{Name: "key", Value: "meta-2"}},
						},
						Stream: bytes.NewBufferString("some-data-2"),
						Release: func() error {
							close(secondReleased)
							return nil
						},
					},
				}

				inputFetcher.FetchStub = func(fetchInputs []builds.Input, fetchEmitter event.Emitter, fetchAbort <-chan struct{}) ([]inputs.FetchedInput, error) {
					Ω(fetchInputs).Should(Equal(build.Inputs))
					Ω(fetchEmitter).Should(Equal(emitter))

					return fetchedInputs, nil
				}
			})

			It("successfully starts", func() {
				Ω(startErr).ShouldNot(HaveOccurred())
			})

			It("creates a container with the specified image", func() {
				created := wardenClient.Connection.CreateArgsForCall(0)
				Ω(created.RootFSPath).Should(Equal("some-rootfs"))
			})

			It("streams them in to the container", func() {
				streamInCalls := wardenClient.Connection.StreamInCallCount()
				Ω(streamInCalls).Should(Equal(2))

				for i := 0; i < streamInCalls; i++ {
					handle, dst, reader := wardenClient.Connection.StreamInArgsForCall(i)
					Ω(handle).Should(Equal("some-handle"))

					in, err := ioutil.ReadAll(reader)
					Ω(err).ShouldNot(HaveOccurred())

					switch string(in) {
					case "some-data-1":
						Ω(dst).Should(Equal("/tmp/build/src/first-resource"))
					case "some-data-2":
						Ω(dst).Should(Equal("/tmp/build/src/second-resource"))
					default:
						Fail("unknown stream destination: " + dst)
					}
				}
			})

			It("releases each resource", func() {
				Ω(firstReleased).Should(BeClosed())
				Ω(secondReleased).Should(BeClosed())
			})

			It("runs the build's script in the container", func() {
				handle, spec, _ := wardenClient.Connection.RunArgsForCall(0)
				Ω(handle).Should(Equal("some-handle"))
				Ω(spec.Path).Should(Equal("./bin/test"))
				Ω(spec.Args).Should(Equal([]string{"arg1", "arg2"}))
				Ω(spec.Env).Should(ConsistOf("FOO=bar", "BAZ=buzz"))
				Ω(spec.Dir).Should(Equal("/tmp/build/src"))
				Ω(spec.TTY).Should(Equal(&warden.TTYSpec{}))
				Ω(spec.Privileged).Should(BeFalse())
			})

			It("emits an initialize event followed by a start event", func() {
				Eventually(events.Sent).Should(ContainElement(event.Initialize{
					BuildConfig: builds.Config{
						Image: "some-rootfs",

						Params: map[string]string{
							"FOO": "bar",
							"BAZ": "buzz",
						},

						Run: builds.RunConfig{
							Path: "./bin/test",
							Args: []string{"arg1", "arg2"},
						},
					},
				}))

				var startEvent event.Start
				Eventually(events.Sent).Should(ContainElement(BeAssignableToTypeOf(startEvent)))

				for _, ev := range events.Sent() {
					switch startEvent := ev.(type) {
					case event.Start:
						Ω(startEvent.Time).Should(BeNumerically("~", time.Now().Unix()))
					}
				}
			})

			Context("when an input provides build configuration", func() {
				BeforeEach(func() {
					fetchedInputs[0].Config = builds.Config{
						Image: "build-config-image",

						Params: map[string]string{
							"FOO":         "build-config-foo",
							"CONFIG_ONLY": "build-config-only",
						},
					}
				})

				It("returns merges the original config over them", func() {
					Ω(started.Build.Config.Image).Should(Equal("some-rootfs"))
					Ω(started.Build.Config.Params).Should(Equal(map[string]string{
						"FOO":         "bar",
						"BAZ":         "buzz",
						"CONFIG_ONLY": "build-config-only",
					}))
				})

				Context("with new input destinations", func() {
					BeforeEach(func() {
						fetchedInputs[0].Config = builds.Config{
							Paths: map[string]string{
								"first-resource":  "reconfigured-first/source/path",
								"second-resource": "reconfigured-second/source/path",
							},
						}
					})

					It("streams them in using the new destinations", func() {
						streamInCalls := wardenClient.Connection.StreamInCallCount()
						Ω(streamInCalls).Should(Equal(2))

						for i := 0; i < streamInCalls; i++ {
							handle, dst, reader := wardenClient.Connection.StreamInArgsForCall(i)
							Ω(handle).Should(Equal("some-handle"))

							in, err := ioutil.ReadAll(reader)
							Ω(err).ShouldNot(HaveOccurred())

							switch string(in) {
							case "some-data-1":
								Ω(dst).Should(Equal("/tmp/build/src/reconfigured-first/source/path"))
							case "some-data-2":
								Ω(dst).Should(Equal("/tmp/build/src/reconfigured-second/source/path"))
							default:
								Fail("unknown stream destination: " + dst)
							}
						}
					})
				})
			})

			Context("when the build is aborted", func() {
				BeforeEach(func() {
					// simulate abort so that start returns it
					inputFetcher.FetchReturns(nil, resource.ErrAborted)
				})

				It("aborts fetching", func() {
					Ω(startErr).Should(Equal(resource.ErrAborted))

					_, _, fetchAbort := inputFetcher.FetchArgsForCall(0)

					Ω(fetchAbort).ShouldNot(BeClosed())

					close(abort)

					Ω(fetchAbort).Should(BeClosed())
				})
			})

			Context("when running the build's script fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					wardenClient.Connection.RunReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(startErr).Should(Equal(disaster))
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to run: oh no!",
					}))
				})
			})

			Context("when privileged is true", func() {
				BeforeEach(func() {
					build.Privileged = true
				})

				It("runs the build privileged", func() {
					handle, spec, _ := wardenClient.Connection.RunArgsForCall(0)
					Ω(handle).Should(Equal("some-handle"))
					Ω(spec.Privileged).Should(BeTrue())
				})
			})

			Context("when the build emits logs", func() {
				BeforeEach(func() {
					wardenClient.Connection.RunStub = func(handle string, spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
						go func() {
							defer GinkgoRecover()

							_, err := io.Stdout.Write([]byte("some stdout data"))
							Ω(err).ShouldNot(HaveOccurred())

							_, err = io.Stderr.Write([]byte("some stderr data"))
							Ω(err).ShouldNot(HaveOccurred())
						}()

						return new(wfakes.FakeProcess), nil
					}
				})

				It("emits a build log event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Log{
						Payload: "some stdout data",
						Origin: event.Origin{
							Type: event.OriginTypeRun,
							Name: "stdout",
						},
					}))

					Eventually(events.Sent).Should(ContainElement(event.Log{
						Payload: "some stderr data",
						Origin: event.Origin{
							Type: event.OriginTypeRun,
							Name: "stderr",
						},
					}))
				})
			})

			Context("when creating the container fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					wardenClient.Connection.CreateReturns("", disaster)
				})

				It("returns the error", func() {
					Ω(startErr).Should(Equal(disaster))
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to create container: oh no!",
					}))
				})
			})

			Context("when copying the source in to the container fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					wardenClient.Connection.StreamInReturns(disaster)
				})

				It("returns the error", func() {
					Ω(startErr).Should(Equal(disaster))
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to stream in resources: oh no!",
					}))
				})
			})

			Describe("after the build starts", func() {
				BeforeEach(func() {
					process := new(wfakes.FakeProcess)
					process.IDReturns(42)
					process.WaitStub = func() (int, error) {
						panic("TODO")
						select {}
					}

					wardenClient.Connection.RunReturns(process, nil)
				})

				It("notifies that the build is started, with updated inputs (version + metadata)", func() {
					inputs := started.Build.Inputs

					Ω(inputs[0].Version).Should(Equal(builds.Version{"version": "1"}))
					Ω(inputs[0].Metadata).Should(Equal([]builds.MetadataField{{Name: "key", Value: "meta-1"}}))

					Ω(inputs[1].Version).Should(Equal(builds.Version{"version": "2"}))
					Ω(inputs[1].Metadata).Should(Equal([]builds.MetadataField{{Name: "key", Value: "meta-2"}}))
				})

				It("returns the container, container handle, process ID, process stream, and logs", func() {
					Ω(started.Container).ShouldNot(BeNil())
					Ω(started.ContainerHandle).Should(Equal("some-handle"))
					Ω(started.ProcessID).Should(Equal(uint32(42)))
					Ω(started.Process).ShouldNot(BeNil())
				})
			})
		})

		Context("when the build has no inputs", func() {
			BeforeEach(func() {
				build.Inputs = nil
			})

			It("executes it in the default directory", func() {
				handle, spec, _ := wardenClient.Connection.RunArgsForCall(0)
				Ω(handle).Should(Equal("some-handle"))
				Ω(spec.Dir).Should(BeEmpty())
			})
		})
	})

	Describe("Attach", func() {
		var exitedBuild ExitedBuild
		var attachErr error

		var runningBuild RunningBuild
		var abort chan struct{}

		JustBeforeEach(func() {
			abort = make(chan struct{})
			exitedBuild, attachErr = builder.Attach(runningBuild, emitter, abort)
		})

		BeforeEach(func() {
			wardenClient.Connection.CreateReturns("the-attached-container", nil)

			container, err := wardenClient.Create(warden.ContainerSpec{})
			Ω(err).ShouldNot(HaveOccurred())

			wardenClient.Connection.CreateReturns("", nil)

			runningProcess := new(wfakes.FakeProcess)

			runningBuild = RunningBuild{
				Build: build,

				ContainerHandle: container.Handle(),
				Container:       container,

				ProcessID: 42,
				Process:   runningProcess,
			}
		})

		Context("when the build's container and process are not present", func() {
			BeforeEach(func() {
				runningBuild.Container = nil
				runningBuild.Process = nil
				wardenClient.Connection.AttachReturns(new(wfakes.FakeProcess), nil)
			})

			Context("and the container can still be found", func() {
				BeforeEach(func() {
					wardenClient.Connection.ListReturns([]string{runningBuild.ContainerHandle}, nil)
				})

				It("looks it up via warden and uses it for attaching", func() {
					Ω(wardenClient.Connection.ListCallCount()).Should(Equal(1))

					handle, pid, _ := wardenClient.Connection.AttachArgsForCall(0)
					Ω(handle).Should(Equal("the-attached-container"))
					Ω(pid).Should(Equal(uint32(42)))
				})
			})

			Context("and the lookup fails", func() {
				BeforeEach(func() {
					wardenClient.Connection.ListReturns([]string{}, nil)
				})

				It("returns an error", func() {
					Ω(attachErr).Should(HaveOccurred())
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to lookup container: container not found: the-attached-container",
					}))
				})
			})
		})

		Context("when the build's process is not present", func() {
			BeforeEach(func() {
				runningBuild.Process = nil
			})

			Context("and attaching succeeds", func() {
				BeforeEach(func() {
					wardenClient.Connection.AttachReturns(new(wfakes.FakeProcess), nil)
				})

				It("attaches to the build's process", func() {
					Ω(wardenClient.Connection.AttachCallCount()).Should(Equal(1))

					handle, pid, _ := wardenClient.Connection.AttachArgsForCall(0)
					Ω(handle).Should(Equal("the-attached-container"))
					Ω(pid).Should(Equal(uint32(42)))
				})

				Context("and the build emits logs", func() {
					BeforeEach(func() {
						wardenClient.Connection.AttachStub = func(handle string, pid uint32, io warden.ProcessIO) (warden.Process, error) {
							Ω(handle).Should(Equal("the-attached-container"))
							Ω(pid).Should(Equal(uint32(42)))
							Ω(io.Stdout).ShouldNot(BeNil())
							Ω(io.Stderr).ShouldNot(BeNil())

							_, err := fmt.Fprintf(io.Stdout, "stdout\n")
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stderr, "stderr\n")
							Ω(err).ShouldNot(HaveOccurred())

							return new(wfakes.FakeProcess), nil
						}
					})

					It("emits log events for stdout/stderr", func() {
						Eventually(events.Sent).Should(ContainElement(event.Log{
							Payload: "stdout\n",
							Origin: event.Origin{
								Type: event.OriginTypeRun,
								Name: "stdout",
							},
						}))

						Eventually(events.Sent).Should(ContainElement(event.Log{
							Payload: "stderr\n",
							Origin: event.Origin{
								Type: event.OriginTypeRun,
								Name: "stderr",
							},
						}))
					})
				})
			})

			Context("and attaching fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					wardenClient.Connection.AttachReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(attachErr).Should(Equal(disaster))
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to attach to process: oh no!",
					}))
				})
			})
		})

		Context("when the build is aborted while the build is running", func() {
			BeforeEach(func() {
				waiting := make(chan struct{})
				stopping := make(chan struct{})

				go func() {
					<-waiting
					close(abort)
				}()

				process := new(wfakes.FakeProcess)
				process.WaitStub = func() (int, error) {
					close(waiting)
					<-stopping
					return 0, nil
				}

				wardenClient.Connection.StopStub = func(string, bool) error {
					close(stopping)
					return nil
				}

				runningBuild.Process = process
			})

			It("stops the container", func() {
				Eventually(wardenClient.Connection.StopCallCount).Should(Equal(1))

				handle, kill := wardenClient.Connection.StopArgsForCall(0)
				Ω(handle).Should(Equal("the-attached-container"))
				Ω(kill).Should(BeFalse())
			})

			It("returns an error", func() {
				Ω(attachErr).Should(HaveOccurred())
			})

			It("emits an error event", func() {
				Eventually(events.Sent).Should(ContainElement(event.Error{
					Message: "result unknown: build aborted",
				}))
			})
		})

		Context("when the build's script exits", func() {
			BeforeEach(func() {
				process := new(wfakes.FakeProcess)
				process.WaitReturns(2, nil)

				runningBuild.Process = process
			})

			It("returns the exited build with the status present", func() {
				Ω(exitedBuild.ExitStatus).Should(Equal(2))
			})
		})
	})

	Describe("Hijack", func() {
		var runningBuild RunningBuild
		var spec warden.ProcessSpec
		var io warden.ProcessIO

		var process warden.Process
		var hijackErr error

		JustBeforeEach(func() {
			process, hijackErr = builder.Hijack(runningBuild, spec, io)
		})

		BeforeEach(func() {
			runningBuild = RunningBuild{
				Build:           build,
				ContainerHandle: "some-handle",
			}

			spec = warden.ProcessSpec{
				Path: "some-path",
				Args: []string{"some", "args"},
			}

			io = warden.ProcessIO{
				Stdin:  new(bytes.Buffer),
				Stdout: new(bytes.Buffer),
			}
		})

		Context("when the container can be found", func() {
			BeforeEach(func() {
				wardenClient.Connection.ListReturns([]string{"some-handle"}, nil)
			})

			Context("and running succeeds", func() {
				var fakeProcess *wfakes.FakeProcess

				BeforeEach(func() {
					fakeProcess = new(wfakes.FakeProcess)
					fakeProcess.WaitReturns(42, nil)

					wardenClient.Connection.RunReturns(fakeProcess, nil)
				})

				It("looks it up via warden and uses it for running", func() {
					Ω(hijackErr).ShouldNot(HaveOccurred())

					Ω(wardenClient.Connection.ListCallCount()).Should(Equal(1))

					ranHandle, ranSpec, ranIO := wardenClient.Connection.RunArgsForCall(0)
					Ω(ranHandle).Should(Equal("some-handle"))
					Ω(ranSpec).Should(Equal(spec))
					Ω(ranIO).Should(Equal(io))
				})

				It("returns the process", func() {
					Ω(process.Wait()).Should(Equal(42))
				})
			})

			Context("and running fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					wardenClient.Connection.RunReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(hijackErr).Should(Equal(disaster))
				})
			})
		})

		Context("when the lookup fails", func() {
			BeforeEach(func() {
				wardenClient.Connection.ListReturns([]string{}, nil)
			})

			It("returns an error", func() {
				Ω(hijackErr).Should(HaveOccurred())
			})
		})
	})

	Describe("Finish", func() {
		var exitedBuild ExitedBuild
		var abort chan struct{}

		var onSuccessOutput builds.Output
		var onSuccessOrFailureOutput builds.Output
		var onFailureOutput builds.Output

		var finished builds.Build
		var finishErr error

		JustBeforeEach(func() {
			abort = make(chan struct{})
			finished, finishErr = builder.Finish(exitedBuild, emitter, abort)
		})

		BeforeEach(func() {
			build.Inputs = []builds.Input{
				{
					Name:    "first-input",
					Type:    "some-type",
					Source:  builds.Source{"uri": "in-source-1"},
					Version: builds.Version{"key": "in-version-1"},
					Metadata: []builds.MetadataField{
						{Name: "first-meta-name", Value: "first-meta-value"},
					},
				},
				{
					Name:    "second-input",
					Type:    "some-type",
					Source:  builds.Source{"uri": "in-source-2"},
					Version: builds.Version{"key": "in-version-2"},
					Metadata: []builds.MetadataField{
						{Name: "second-meta-name", Value: "second-meta-value"},
					},
				},
			}

			onSuccessOutput = builds.Output{
				Name:   "on-success",
				Type:   "some-type",
				On:     []builds.OutputCondition{builds.OutputConditionSuccess},
				Params: builds.Params{"key": "success-param"},
				Source: builds.Source{"uri": "http://success-uri"},
			}

			onSuccessOrFailureOutput = builds.Output{
				Name: "on-success-or-failure",
				Type: "some-type",
				On: []builds.OutputCondition{
					builds.OutputConditionSuccess,
					builds.OutputConditionFailure,
				},
				Params: builds.Params{"key": "success-or-failure-param"},
				Source: builds.Source{"uri": "http://success-or-failure-uri"},
			}

			onFailureOutput = builds.Output{
				Name: "on-failure",
				Type: "some-type",
				On: []builds.OutputCondition{
					builds.OutputConditionFailure,
				},
				Params: builds.Params{"key": "failure-param"},
				Source: builds.Source{"uri": "http://failure-uri"},
			}

			build.Outputs = []builds.Output{
				onSuccessOutput,
				onSuccessOrFailureOutput,
				onFailureOutput,
			}

			wardenClient.Connection.CreateReturns("the-attached-container", nil)

			container, err := wardenClient.Create(warden.ContainerSpec{})
			Ω(err).ShouldNot(HaveOccurred())

			wardenClient.Connection.CreateReturns("", nil)

			exitedBuild = ExitedBuild{
				Build: build,

				Container: container,
			}
		})

		Context("when the build exited with success", func() {
			BeforeEach(func() {
				exitedBuild.ExitStatus = 0
			})

			It("emits a Finish event", func() {
				var finishEvent event.Finish
				Eventually(events.Sent).Should(ContainElement(BeAssignableToTypeOf(finishEvent)))

				for _, ev := range events.Sent() {
					switch finishEvent := ev.(type) {
					case event.Finish:
						Ω(finishEvent.ExitStatus).Should(Equal(0))
						Ω(finishEvent.Time).Should(BeNumerically("~", time.Now().Unix()))
					}
				}
			})

			It("performs the set of 'on success' outputs", func() {
				Ω(outputPerformer.PerformOutputsCallCount()).Should(Equal(1))

				container, outputs, performingEmitter, _ := outputPerformer.PerformOutputsArgsForCall(0)
				Ω(container).Should(Equal(exitedBuild.Container))
				Ω(outputs).Should(Equal([]builds.Output{
					onSuccessOutput,
					onSuccessOrFailureOutput,
				}))
				Ω(performingEmitter).Should(Equal(emitter))
			})

			Context("when the build is aborted", func() {
				It("aborts performing outputs", func() {
					_, _, _, performingAbort := outputPerformer.PerformOutputsArgsForCall(0)

					Ω(performingAbort).ShouldNot(BeClosed())

					close(abort)

					Ω(performingAbort).Should(BeClosed())
				})
			})

			Context("when performing outputs succeeds", func() {
				explicitOutputOnSuccess := builds.Output{
					Name:     "on-success",
					Type:     "some-type",
					On:       []builds.OutputCondition{builds.OutputConditionSuccess},
					Source:   builds.Source{"uri": "http://success-uri"},
					Params:   builds.Params{"key": "success-param"},
					Version:  builds.Version{"version": "on-success-performed"},
					Metadata: []builds.MetadataField{{Name: "output", Value: "on-success"}},
				}

				explicitOutputOnSuccessOrFailure := builds.Output{
					Name: "on-success-or-failure",
					Type: "some-type",
					On: []builds.OutputCondition{
						builds.OutputConditionSuccess,
						builds.OutputConditionFailure,
					},
					Source:   builds.Source{"uri": "http://success-or-failure-uri"},
					Params:   builds.Params{"key": "success-or-failure-param"},
					Version:  builds.Version{"version": "on-success-or-failure-performed"},
					Metadata: []builds.MetadataField{{Name: "output", Value: "on-success-or-failure"}},
				}

				BeforeEach(func() {
					performedOutputs := []builds.Output{
						explicitOutputOnSuccess,
						explicitOutputOnSuccessOrFailure,
					}

					outputPerformer.PerformOutputsReturns(performedOutputs, nil)
				})

				It("returns the performed outputs", func() {
					Ω(finished.Outputs).Should(HaveLen(2))

					Ω(finished.Outputs).Should(ContainElement(explicitOutputOnSuccess))
					Ω(finished.Outputs).Should(ContainElement(explicitOutputOnSuccessOrFailure))
				})
			})

			Context("when performing outputs fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					outputPerformer.PerformOutputsReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(finishErr).Should(Equal(disaster))
				})
			})
		})

		Context("when the build exited with failure", func() {
			BeforeEach(func() {
				exitedBuild.ExitStatus = 2
			})

			It("emits a Finish event", func() {
				var finishEvent event.Finish
				Eventually(events.Sent).Should(ContainElement(BeAssignableToTypeOf(finishEvent)))

				for _, ev := range events.Sent() {
					switch finishEvent := ev.(type) {
					case event.Finish:
						Ω(finishEvent.ExitStatus).Should(Equal(2))
						Ω(finishEvent.Time).Should(BeNumerically("~", time.Now().Unix()))
					}
				}
			})

			It("performs the set of 'on failure' outputs", func() {
				Ω(outputPerformer.PerformOutputsCallCount()).Should(Equal(1))

				container, outputs, performingEmitter, _ := outputPerformer.PerformOutputsArgsForCall(0)
				Ω(container).Should(Equal(exitedBuild.Container))
				Ω(outputs).Should(Equal([]builds.Output{
					onSuccessOrFailureOutput,
					onFailureOutput,
				}))
				Ω(performingEmitter).Should(Equal(emitter))
			})

			Context("when the build is aborted", func() {
				It("aborts performing outputs", func() {
					_, _, _, performingAbort := outputPerformer.PerformOutputsArgsForCall(0)

					Ω(performingAbort).ShouldNot(BeClosed())

					close(abort)

					Ω(performingAbort).Should(BeClosed())
				})
			})

			Context("when performing outputs succeeds", func() {
				explicitOutputOnSuccessOrFailure := builds.Output{
					Name: "on-success-or-failure",
					Type: "some-type",
					On: []builds.OutputCondition{
						builds.OutputConditionSuccess,
						builds.OutputConditionFailure,
					},
					Source:   builds.Source{"uri": "http://success-or-failure-uri"},
					Params:   builds.Params{"key": "success-or-failure-param"},
					Version:  builds.Version{"version": "on-success-or-failure-performed"},
					Metadata: []builds.MetadataField{{Name: "output", Value: "on-success-or-failure"}},
				}

				explicitOutputOnFailure := builds.Output{
					Name:     "on-failure",
					Type:     "some-type",
					On:       []builds.OutputCondition{builds.OutputConditionSuccess},
					Source:   builds.Source{"uri": "http://failure-uri"},
					Params:   builds.Params{"key": "failure-param"},
					Version:  builds.Version{"version": "on-failure-performed"},
					Metadata: []builds.MetadataField{{Name: "output", Value: "on-failure"}},
				}

				BeforeEach(func() {
					performedOutputs := []builds.Output{
						explicitOutputOnSuccessOrFailure,
						explicitOutputOnFailure,
					}

					outputPerformer.PerformOutputsReturns(performedOutputs, nil)
				})

				It("returns the explicitly-performed outputs", func() {
					Ω(finished.Outputs).Should(HaveLen(2))

					Ω(finished.Outputs).Should(ContainElement(explicitOutputOnSuccessOrFailure))
					Ω(finished.Outputs).Should(ContainElement(explicitOutputOnFailure))
				})
			})

			Context("when performing outputs fails", func() {
				disaster := errors.New("oh no!")

				BeforeEach(func() {
					outputPerformer.PerformOutputsReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(finishErr).Should(Equal(disaster))
				})
			})
		})
	})
})
