package builder_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"time"

	garden "github.com/cloudfoundry-incubator/garden/api"
	gfakes "github.com/cloudfoundry-incubator/garden/api/fakes"
	"github.com/cloudfoundry-incubator/garden/client/fake_api_client"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/concourse/turbine"
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
		gardenClient    *fake_api_client.FakeClient
		inputFetcher    *ifakes.FakeFetcher
		outputPerformer *ofakes.FakePerformer

		emitter *efakes.FakeEmitter
		events  *testlog.EventLog

		builder Builder

		build turbine.Build
	)

	BeforeEach(func() {
		gardenClient = fake_api_client.New()

		emitter = new(efakes.FakeEmitter)

		events = &testlog.EventLog{}
		emitter.EmitEventStub = events.Add

		inputFetcher = new(ifakes.FakeFetcher)
		outputPerformer = new(ofakes.FakePerformer)

		builder = NewBuilder(gardenClient, inputFetcher, outputPerformer)

		build = turbine.Build{
			Guid: "some-build-guid",

			Config: turbine.Config{
				Image: "some-rootfs",

				Params: map[string]string{
					"FOO": "bar",
					"BAZ": "buzz",
				},

				Run: turbine.RunConfig{
					Path: "./bin/test",
					Args: []string{"arg1", "arg2"},
				},
			},
		}

		gardenClient.Connection.CreateReturns("some-build-guid", nil)
	})

	Describe("Start", func() {
		var (
			started  RunningBuild
			startErr error
		)

		BeforeEach(func() {
			build.Inputs = []turbine.Input{
				{
					Name: "first-resource",
					Type: "raw",
				},
				{
					Name: "second-resource",
					Type: "raw",
				},
			}

			runningProcess := new(gfakes.FakeProcess)
			runningProcess.IDReturns(42)

			gardenClient.Connection.RunReturns(runningProcess, nil)
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
						Input: turbine.Input{
							Name:     "first-resource",
							Type:     "raw",
							Version:  turbine.Version{"version": "1"},
							Metadata: []turbine.MetadataField{{Name: "key", Value: "meta-1"}},
						},
						Stream: bytes.NewBufferString("some-data-1"),
						Release: func() error {
							close(firstReleased)
							return nil
						},
					},
					{
						Input: turbine.Input{
							Name:     "second-resource",
							Type:     "raw",
							Version:  turbine.Version{"version": "2"},
							Metadata: []turbine.MetadataField{{Name: "key", Value: "meta-2"}},
						},
						Stream: bytes.NewBufferString("some-data-2"),
						Release: func() error {
							close(secondReleased)
							return nil
						},
					},
				}

				inputFetcher.FetchStub = func(fetchInputs []turbine.Input, fetchEmitter event.Emitter, fetchAbort <-chan struct{}) ([]inputs.FetchedInput, error) {
					Ω(fetchInputs).Should(Equal(build.Inputs))
					Ω(fetchEmitter).Should(Equal(emitter))

					return fetchedInputs, nil
				}
			})

			It("successfully starts", func() {
				Ω(startErr).ShouldNot(HaveOccurred())
			})

			It("creates a container with the specified image", func() {
				created := gardenClient.Connection.CreateArgsForCall(0)
				Ω(created.RootFSPath).Should(Equal("some-rootfs"))
			})

			It("creates a container with the build's guid as the handle", func() {
				created := gardenClient.Connection.CreateArgsForCall(0)
				Ω(created.Handle).ShouldNot(BeEmpty())
				Ω(created.Handle).Should(Equal(build.Guid))
			})

			It("creates a container unprivileged", func() {
				created := gardenClient.Connection.CreateArgsForCall(0)
				Ω(created.Privileged).Should(BeFalse())
			})

			It("streams them in to the container", func() {
				streamInCalls := gardenClient.Connection.StreamInCallCount()
				Ω(streamInCalls).Should(Equal(2))

				for i := 0; i < streamInCalls; i++ {
					handle, dst, reader := gardenClient.Connection.StreamInArgsForCall(i)
					Ω(handle).Should(Equal("some-build-guid"))

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
				handle, spec, _ := gardenClient.Connection.RunArgsForCall(0)
				Ω(handle).Should(Equal("some-build-guid"))
				Ω(spec.Path).Should(Equal("./bin/test"))
				Ω(spec.Args).Should(Equal([]string{"arg1", "arg2"}))
				Ω(spec.Env).Should(ConsistOf("FOO=bar", "BAZ=buzz"))
				Ω(spec.Dir).Should(Equal("/tmp/build/src"))
				Ω(spec.TTY).Should(Equal(&garden.TTYSpec{}))
				Ω(spec.Privileged).Should(BeFalse())
			})

			It("emits an initialize event followed by a start event", func() {
				Eventually(events.Sent).Should(ContainElement(event.Initialize{
					BuildConfig: turbine.Config{
						Image: "some-rootfs",

						Params: map[string]string{
							"FOO": "bar",
							"BAZ": "buzz",
						},

						Run: turbine.RunConfig{
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

			Context("when the build has not configured a container image", func() {
				BeforeEach(func() {
					build.Config.Image = ""
				})

				It("returns an error", func() {
					Ω(startErr).Should(Equal(ErrNoImageSpecified))
				})

				It("errors", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to create container: no image specified",
					}))
				})

				Context("but an input configured an image", func() {
					BeforeEach(func() {
						fetchedInputs[0].Config = turbine.Config{
							Image: "build-config-image",
						}
					})

					It("successfully starts", func() {
						Ω(startErr).ShouldNot(HaveOccurred())
					})

					It("successfully creates the container", func() {
						created := gardenClient.Connection.CreateArgsForCall(0)
						Ω(created.RootFSPath).Should(Equal("build-config-image"))
					})
				})
			})

			Context("when an input provides build configuration", func() {
				BeforeEach(func() {
					fetchedInputs[0].Config = turbine.Config{
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

				Context("which specifies explicit inputs", func() {
					BeforeEach(func() {
						fetchedInputs[0].Config = turbine.Config{
							Inputs: []turbine.InputConfig{
								{
									Name: "first-resource",
									Path: "first/source/path",
								},
								{
									Name: "second-resource",
								},
							},
						}
					})

					It("streams them in using the new destinations", func() {
						streamInCalls := gardenClient.Connection.StreamInCallCount()
						Ω(streamInCalls).Should(Equal(2))

						for i := 0; i < streamInCalls; i++ {
							handle, dst, reader := gardenClient.Connection.StreamInArgsForCall(i)
							Ω(handle).Should(Equal("some-build-guid"))

							in, err := ioutil.ReadAll(reader)
							Ω(err).ShouldNot(HaveOccurred())

							switch string(in) {
							case "some-data-1":
								Ω(dst).Should(Equal("/tmp/build/src/first/source/path"))
							case "some-data-2":
								Ω(dst).Should(Equal("/tmp/build/src/second-resource"))
							default:
								Fail("unknown stream destination: " + dst)
							}
						}
					})

					Context("and some are missing in the build", func() {
						BeforeEach(func() {
							fetchedInputs[0].Config.Inputs = []turbine.InputConfig{
								{Name: "some-bogus-input"},
							}
						})

						It("returns an error", func() {
							Ω(startErr).Should(Equal(UnsatisfiedInputError{"some-bogus-input"}))
						})
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
					gardenClient.Connection.RunReturns(nil, disaster)
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

				It("creates the container with privileged true", func() {
					created := gardenClient.Connection.CreateArgsForCall(0)
					Ω(created.Privileged).Should(BeTrue())
				})

				It("runs the build privileged", func() {
					handle, spec, _ := gardenClient.Connection.RunArgsForCall(0)
					Ω(handle).Should(Equal("some-build-guid"))
					Ω(spec.Privileged).Should(BeTrue())
				})
			})

			Context("when the build emits logs", func() {
				BeforeEach(func() {
					gardenClient.Connection.RunStub = func(handle string, spec garden.ProcessSpec, io garden.ProcessIO) (garden.Process, error) {
						go func() {
							defer GinkgoRecover()

							_, err := io.Stdout.Write([]byte("some stdout data"))
							Ω(err).ShouldNot(HaveOccurred())

							_, err = io.Stderr.Write([]byte("some stderr data"))
							Ω(err).ShouldNot(HaveOccurred())
						}()

						return new(gfakes.FakeProcess), nil
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
					gardenClient.Connection.CreateReturns("", disaster)
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
					gardenClient.Connection.StreamInReturns(disaster)
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
					process := new(gfakes.FakeProcess)
					process.IDReturns(42)
					process.WaitStub = func() (int, error) {
						panic("TODO")
						select {}
					}

					gardenClient.Connection.RunReturns(process, nil)
				})

				It("notifies that the build is started, with updated inputs (version + metadata)", func() {
					inputs := started.Build.Inputs

					Ω(inputs[0].Version).Should(Equal(turbine.Version{"version": "1"}))
					Ω(inputs[0].Metadata).Should(Equal([]turbine.MetadataField{{Name: "key", Value: "meta-1"}}))

					Ω(inputs[1].Version).Should(Equal(turbine.Version{"version": "2"}))
					Ω(inputs[1].Metadata).Should(Equal([]turbine.MetadataField{{Name: "key", Value: "meta-2"}}))
				})

				It("returns the container, container handle, process ID, process stream, and logs", func() {
					Ω(started.Container).ShouldNot(BeNil())
					Ω(started.ProcessID).Should(Equal(uint32(42)))
					Ω(started.Process).ShouldNot(BeNil())
				})
			})
		})

		Context("when the build has no inputs", func() {
			BeforeEach(func() {
				build.Inputs = nil
			})

			It("streams an empty tarball in to /tmp/build/src", func() {
				streamInCalls := gardenClient.Connection.StreamInCallCount()
				Ω(streamInCalls).Should(Equal(1))

				handle, dst, reader := gardenClient.Connection.StreamInArgsForCall(0)
				Ω(handle).Should(Equal("some-build-guid"))
				Ω(dst).Should(Equal("/tmp/build/src"))

				tarReader := tar.NewReader(reader)

				_, err := tarReader.Next()
				Ω(err).Should(Equal(io.EOF))
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
			container, err := gardenClient.Create(garden.ContainerSpec{})
			Ω(err).ShouldNot(HaveOccurred())

			runningProcess := new(gfakes.FakeProcess)

			runningBuild = RunningBuild{
				Build: build,

				Container: container,

				ProcessID: 42,
				Process:   runningProcess,
			}
		})

		Context("when the build's container and process are not present", func() {
			BeforeEach(func() {
				runningBuild.Container = nil
				runningBuild.Process = nil
				gardenClient.Connection.AttachReturns(new(gfakes.FakeProcess), nil)
			})

			Context("and the container can still be found", func() {
				BeforeEach(func() {
					gardenClient.Connection.ListReturns([]string{runningBuild.Build.Guid}, nil)
				})

				It("looks it up via garden and uses it for attaching", func() {
					Ω(gardenClient.Connection.ListCallCount()).Should(Equal(1))

					handle, pid, _ := gardenClient.Connection.AttachArgsForCall(0)
					Ω(handle).Should(Equal("some-build-guid"))
					Ω(pid).Should(Equal(uint32(42)))
				})
			})

			Context("and the lookup fails", func() {
				BeforeEach(func() {
					gardenClient.Connection.ListReturns([]string{}, nil)
				})

				It("returns an error", func() {
					Ω(attachErr).Should(HaveOccurred())
				})

				It("emits an error event", func() {
					Eventually(events.Sent).Should(ContainElement(event.Error{
						Message: "failed to lookup container: container not found",
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
					gardenClient.Connection.AttachReturns(new(gfakes.FakeProcess), nil)
				})

				It("attaches to the build's process", func() {
					Ω(gardenClient.Connection.AttachCallCount()).Should(Equal(1))

					handle, pid, _ := gardenClient.Connection.AttachArgsForCall(0)
					Ω(handle).Should(Equal("some-build-guid"))
					Ω(pid).Should(Equal(uint32(42)))
				})

				Context("and the build emits logs", func() {
					BeforeEach(func() {
						gardenClient.Connection.AttachStub = func(handle string, pid uint32, io garden.ProcessIO) (garden.Process, error) {
							Ω(handle).Should(Equal("some-build-guid"))
							Ω(pid).Should(Equal(uint32(42)))
							Ω(io.Stdout).ShouldNot(BeNil())
							Ω(io.Stderr).ShouldNot(BeNil())

							_, err := fmt.Fprintf(io.Stdout, "stdout\n")
							Ω(err).ShouldNot(HaveOccurred())

							_, err = fmt.Fprintf(io.Stderr, "stderr\n")
							Ω(err).ShouldNot(HaveOccurred())

							return new(gfakes.FakeProcess), nil
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
					gardenClient.Connection.AttachReturns(nil, disaster)
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

				process := new(gfakes.FakeProcess)
				process.WaitStub = func() (int, error) {
					close(waiting)
					<-stopping
					return 0, nil
				}

				gardenClient.Connection.StopStub = func(string, bool) error {
					close(stopping)
					return nil
				}

				runningBuild.Process = process
			})

			It("stops the container", func() {
				Eventually(gardenClient.Connection.StopCallCount).Should(Equal(1))

				handle, kill := gardenClient.Connection.StopArgsForCall(0)
				Ω(handle).Should(Equal("some-build-guid"))
				Ω(kill).Should(BeFalse())
			})

			It("returns an error", func() {
				Ω(attachErr).Should(HaveOccurred())
			})

			It("emits an error event", func() {
				Eventually(events.Sent).Should(ContainElement(event.Error{
					Message: "running failed: build aborted",
				}))
			})
		})

		Context("when the build's script exits", func() {
			BeforeEach(func() {
				process := new(gfakes.FakeProcess)
				process.WaitReturns(2, nil)

				runningBuild.Process = process
			})

			It("returns the exited build with the status present", func() {
				Ω(exitedBuild.ExitStatus).Should(Equal(2))
			})
		})
	})

	Describe("Hijack", func() {
		var spec garden.ProcessSpec
		var io garden.ProcessIO

		var process garden.Process
		var hijackErr error

		JustBeforeEach(func() {
			process, hijackErr = builder.Hijack("some-build-guid", spec, io)
		})

		BeforeEach(func() {
			spec = garden.ProcessSpec{
				Path: "some-path",
				Args: []string{"some", "args"},
			}

			io = garden.ProcessIO{
				Stdin:  new(bytes.Buffer),
				Stdout: new(bytes.Buffer),
			}
		})

		Context("when the container can be found", func() {
			BeforeEach(func() {
				gardenClient.Connection.ListReturns([]string{"some-build-guid"}, nil)
			})

			Context("and running succeeds", func() {
				var fakeProcess *gfakes.FakeProcess

				BeforeEach(func() {
					fakeProcess = new(gfakes.FakeProcess)
					fakeProcess.WaitReturns(42, nil)

					gardenClient.Connection.RunReturns(fakeProcess, nil)
				})

				It("looks it up via garden and uses it for running", func() {
					Ω(hijackErr).ShouldNot(HaveOccurred())

					Ω(gardenClient.Connection.ListCallCount()).Should(Equal(1))

					ranHandle, ranSpec, ranIO := gardenClient.Connection.RunArgsForCall(0)
					Ω(ranHandle).Should(Equal("some-build-guid"))
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
					gardenClient.Connection.RunReturns(nil, disaster)
				})

				It("returns the error", func() {
					Ω(hijackErr).Should(Equal(disaster))
				})
			})
		})

		Context("when the lookup fails", func() {
			BeforeEach(func() {
				gardenClient.Connection.ListReturns([]string{}, nil)
			})

			It("returns an error", func() {
				Ω(hijackErr).Should(HaveOccurred())
			})
		})
	})

	Describe("Finish", func() {
		var exitedBuild ExitedBuild
		var abort chan struct{}

		var onSuccessOutput turbine.Output
		var onSuccessOrFailureOutput turbine.Output
		var onFailureOutput turbine.Output

		var finished turbine.Build
		var finishErr error

		JustBeforeEach(func() {
			abort = make(chan struct{})
			finished, finishErr = builder.Finish(exitedBuild, emitter, abort)
		})

		BeforeEach(func() {
			build.Inputs = []turbine.Input{
				{
					Name:    "first-input",
					Type:    "some-type",
					Source:  turbine.Source{"uri": "in-source-1"},
					Version: turbine.Version{"key": "in-version-1"},
					Metadata: []turbine.MetadataField{
						{Name: "first-meta-name", Value: "first-meta-value"},
					},
				},
				{
					Name:    "second-input",
					Type:    "some-type",
					Source:  turbine.Source{"uri": "in-source-2"},
					Version: turbine.Version{"key": "in-version-2"},
					Metadata: []turbine.MetadataField{
						{Name: "second-meta-name", Value: "second-meta-value"},
					},
				},
			}

			onSuccessOutput = turbine.Output{
				Name:   "on-success",
				Type:   "some-type",
				On:     []turbine.OutputCondition{turbine.OutputConditionSuccess},
				Params: turbine.Params{"key": "success-param"},
				Source: turbine.Source{"uri": "http://success-uri"},
			}

			onSuccessOrFailureOutput = turbine.Output{
				Name: "on-success-or-failure",
				Type: "some-type",
				On: []turbine.OutputCondition{
					turbine.OutputConditionSuccess,
					turbine.OutputConditionFailure,
				},
				Params: turbine.Params{"key": "success-or-failure-param"},
				Source: turbine.Source{"uri": "http://success-or-failure-uri"},
			}

			onFailureOutput = turbine.Output{
				Name: "on-failure",
				Type: "some-type",
				On: []turbine.OutputCondition{
					turbine.OutputConditionFailure,
				},
				Params: turbine.Params{"key": "failure-param"},
				Source: turbine.Source{"uri": "http://failure-uri"},
			}

			build.Outputs = []turbine.Output{
				onSuccessOutput,
				onSuccessOrFailureOutput,
				onFailureOutput,
			}

			container, err := gardenClient.Create(garden.ContainerSpec{})
			Ω(err).ShouldNot(HaveOccurred())

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
				Ω(outputs).Should(Equal([]turbine.Output{
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
				explicitOutputOnSuccess := turbine.Output{
					Name:     "on-success",
					Type:     "some-type",
					On:       []turbine.OutputCondition{turbine.OutputConditionSuccess},
					Source:   turbine.Source{"uri": "http://success-uri"},
					Params:   turbine.Params{"key": "success-param"},
					Version:  turbine.Version{"version": "on-success-performed"},
					Metadata: []turbine.MetadataField{{Name: "output", Value: "on-success"}},
				}

				explicitOutputOnSuccessOrFailure := turbine.Output{
					Name: "on-success-or-failure",
					Type: "some-type",
					On: []turbine.OutputCondition{
						turbine.OutputConditionSuccess,
						turbine.OutputConditionFailure,
					},
					Source:   turbine.Source{"uri": "http://success-or-failure-uri"},
					Params:   turbine.Params{"key": "success-or-failure-param"},
					Version:  turbine.Version{"version": "on-success-or-failure-performed"},
					Metadata: []turbine.MetadataField{{Name: "output", Value: "on-success-or-failure"}},
				}

				BeforeEach(func() {
					performedOutputs := []turbine.Output{
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
				Ω(outputs).Should(Equal([]turbine.Output{
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
				explicitOutputOnSuccessOrFailure := turbine.Output{
					Name: "on-success-or-failure",
					Type: "some-type",
					On: []turbine.OutputCondition{
						turbine.OutputConditionSuccess,
						turbine.OutputConditionFailure,
					},
					Source:   turbine.Source{"uri": "http://success-or-failure-uri"},
					Params:   turbine.Params{"key": "success-or-failure-param"},
					Version:  turbine.Version{"version": "on-success-or-failure-performed"},
					Metadata: []turbine.MetadataField{{Name: "output", Value: "on-success-or-failure"}},
				}

				explicitOutputOnFailure := turbine.Output{
					Name:     "on-failure",
					Type:     "some-type",
					On:       []turbine.OutputCondition{turbine.OutputConditionSuccess},
					Source:   turbine.Source{"uri": "http://failure-uri"},
					Params:   turbine.Params{"key": "failure-param"},
					Version:  turbine.Version{"version": "on-failure-performed"},
					Metadata: []turbine.MetadataField{{Name: "output", Value: "on-failure"}},
				}

				BeforeEach(func() {
					performedOutputs := []turbine.Output{
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
