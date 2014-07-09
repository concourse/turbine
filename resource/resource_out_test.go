package resource_test

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"

	"github.com/cloudfoundry-incubator/garden/warden"
	wfakes "github.com/cloudfoundry-incubator/garden/warden/fakes"
	"github.com/concourse/turbine/api/builds"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
)

var _ = Describe("Resource Out", func() {
	var (
		output builds.Output

		outScriptStdout     string
		outScriptStderr     string
		outScriptExitStatus int
		outScriptError      error

		outOutput builds.Output
		outErr    error
	)

	BeforeEach(func() {
		output = builds.Output{
			Type:   "some-resource",
			Params: builds.Params{"some": "params"},
			Source: builds.Source{"some": "source"},

			SourcePath: "some-resource",
		}

		outScriptStdout = "{}"
		outScriptStderr = ""
		outScriptExitStatus = 0
		outScriptError = nil
	})

	JustBeforeEach(func() {
		wardenClient.Connection.RunStub = func(handle string, spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
			if outScriptError != nil {
				return nil, outScriptError
			}

			_, err := io.Stdout.Write([]byte(outScriptStdout))
			Ω(err).ShouldNot(HaveOccurred())

			_, err = io.Stderr.Write([]byte(outScriptStderr))
			Ω(err).ShouldNot(HaveOccurred())

			process := new(wfakes.FakeProcess)
			process.WaitReturns(outScriptExitStatus, nil)

			return process, nil
		}

		outOutput, outErr = resource.Out(bytes.NewBufferString("the-source"), output)
	})

	It("runs /tmp/resource/out <source path> with the request on stdin", func() {
		Ω(outErr).ShouldNot(HaveOccurred())

		handle, spec, io := wardenClient.Connection.RunArgsForCall(0)
		Ω(handle).Should(Equal("some-handle"))
		Ω(spec.Path).Should(Equal("/tmp/resource/out"))
		Ω(spec.Args).Should(Equal([]string{"/tmp/build/src"}))
		Ω(spec.Privileged).Should(BeTrue())

		request, err := ioutil.ReadAll(io.Stdin)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(string(request)).Should(Equal(`{"params":{"some":"params"},"source":{"some":"source"}}`))
	})

	Context("when streaming the source in succeeds", func() {
		var streamedIn *gbytes.Buffer

		BeforeEach(func() {
			streamedIn = gbytes.NewBuffer()

			wardenClient.Connection.StreamInStub = func(handle string, destination string, source io.Reader) error {
				Ω(handle).Should(Equal("some-handle"))

				if destination == "/tmp/build/src" {
					_, err := io.Copy(streamedIn, source)
					Ω(err).ShouldNot(HaveOccurred())
				}

				return nil
			}
		})

		It("writes the stream source to the destination", func() {
			Ω(outErr).ShouldNot(HaveOccurred())

			Ω(string(streamedIn.Contents())).Should(Equal("the-source"))
		})
	})

	Context("when /tmp/resource/out prints the version and metadata", func() {
		BeforeEach(func() {
			outScriptStdout = `{
				"version": {"some": "new-version"},
				"metadata": [
					{"name": "a", "value":"a-value"},
					{"name": "b","value": "b-value"}
				]
			}`
		})

		It("returns the build source printed out by /tmp/resource/out", func() {
			expectedOutput := output
			expectedOutput.Version = builds.Version{"some": "new-version"}
			expectedOutput.Metadata = []builds.MetadataField{
				{Name: "a", Value: "a-value"},
				{Name: "b", Value: "b-value"},
			}

			Ω(outOutput).Should(Equal(expectedOutput))
		})
	})

	Context("when /out outputs to stderr", func() {
		BeforeEach(func() {
			outScriptStderr = "some stderr data"
		})

		It("emits it to the log sink", func() {
			Ω(outErr).ShouldNot(HaveOccurred())

			Ω(string(logs.Contents())).Should(Equal("some stderr data"))
		})
	})

	Context("when streaming in the source fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			wardenClient.Connection.StreamInReturns(disaster)
		})

		It("returns the error", func() {
			Ω(outErr).Should(Equal(disaster))
		})
	})

	Context("when running /tmp/resource/out fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			outScriptError = disaster
		})

		It("returns the error", func() {
			Ω(outErr).Should(Equal(disaster))
		})
	})

	Context("when /tmp/resource/out exits nonzero", func() {
		BeforeEach(func() {
			outScriptStdout = "some-stdout-data"
			outScriptStderr = "some-stderr-data"
			outScriptExitStatus = 9
		})

		It("returns an err containing stdout/stderr of the process", func() {
			Ω(outErr).Should(HaveOccurred())
			Ω(outErr.Error()).Should(ContainSubstring("some-stdout-data"))
			Ω(outErr.Error()).Should(ContainSubstring("some-stderr-data"))
			Ω(outErr.Error()).Should(ContainSubstring("exit status 9"))
		})
	})

	Context("when aborting", func() {
		BeforeEach(func() {
			wardenClient.Connection.RunStub = func(handle string, spec warden.ProcessSpec, io warden.ProcessIO) (warden.Process, error) {
				process := new(wfakes.FakeProcess)
				process.WaitStub = func() (int, error) {
					// cause waiting to block so that it can be aborted
					select {}
					return 0, nil
				}

				return process, nil
			}
		})

		It("stops the container", func() {
			go resource.Out(bytes.NewBufferString("the-source"), output)

			close(abort)

			Eventually(wardenClient.Connection.StopCallCount).Should(Equal(1))

			handle, kill := wardenClient.Connection.StopArgsForCall(0)
			Ω(handle).Should(Equal("some-handle"))
			Ω(kill).Should(BeFalse())
		})
	})
})
