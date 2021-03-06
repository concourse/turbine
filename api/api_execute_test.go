package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/concourse/turbine"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("POST /builds", func() {
	var build turbine.Build
	var requestBody string
	var response *http.Response

	buildPayload := func(build turbine.Build) string {
		payload, err := json.Marshal(build)
		Ω(err).ShouldNot(HaveOccurred())

		return string(payload)
	}

	BeforeEach(func() {
		build = turbine.Build{
			Inputs: []turbine.Input{
				{
					Type: "git",
				},
			},
		}

		requestBody = buildPayload(build)
	})

	JustBeforeEach(func() {
		var err error

		response, err = client.Post(
			server.URL+"/builds",
			"application/json",
			bytes.NewBufferString(requestBody),
		)
		Ω(err).ShouldNot(HaveOccurred())
	})

	It("returns 201", func() {
		Ω(response.StatusCode).Should(Equal(http.StatusCreated))
	})

	It("schedules the build and returns it with a guid and abort url", func() {
		var returnedBuild turbine.Build

		err := json.NewDecoder(response.Body).Decode(&returnedBuild)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(returnedBuild.Guid).ShouldNot(BeEmpty())
		Ω(returnedBuild.Inputs).Should(Equal(build.Inputs))

		Ω(scheduler.StartCallCount()).Should(Equal(1))
		Ω(scheduler.StartArgsForCall(0)).Should(Equal(returnedBuild))
	})

	It("returns the turbine endpoint as X-Turbine-Endpoint", func() {
		Ω(response.Header.Get("X-Turbine-Endpoint")).Should(Equal("http://some-turbine"))
	})

	Context("when the payload is malformed JSON", func() {
		BeforeEach(func() {
			requestBody = "ß"
		})

		It("returns 400", func() {
			Ω(response.StatusCode).Should(Equal(http.StatusBadRequest))
		})
	})
})
