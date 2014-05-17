package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/winston-ci/prole/api"
	"github.com/winston-ci/prole/api/builds"
	"github.com/winston-ci/prole/checker/fakechecker"
	"github.com/winston-ci/prole/scheduler/fakescheduler"
)

var _ = Describe("API", func() {
	var checker *fakechecker.FakeChecker

	var server *httptest.Server
	var client *http.Client

	BeforeEach(func() {
		scheduler := fakescheduler.New()
		checker = fakechecker.New()

		handler, err := api.New(scheduler, checker)
		Ω(err).ShouldNot(HaveOccurred())

		server = httptest.NewServer(handler)
		client = &http.Client{
			Transport: &http.Transport{},
		}
	})

	Describe("POST /checks", func() {
		var input builds.Input
		var requestBody string
		var response *http.Response

		inputPayload := func(input builds.Input) string {
			payload, err := json.Marshal(input)
			Ω(err).ShouldNot(HaveOccurred())

			return string(payload)
		}

		BeforeEach(func() {
			version := json.RawMessage(`{"ref":"foo"}`)

			input = builds.Input{
				Type:    "git",
				Version: &version,
			}

			requestBody = inputPayload(input)
		})

		JustBeforeEach(func() {
			var err error

			response, err = client.Post(
				server.URL+"/checks",
				"application/json",
				bytes.NewBufferString(requestBody),
			)
			Ω(err).ShouldNot(HaveOccurred())
		})

		It("returns 200", func() {
			Ω(response.StatusCode).Should(Equal(http.StatusOK))
		})

		It("checks for new versions of the given input", func() {
			Ω(checker.Checked()).Should(ContainElement(input))
		})

		Context("when the check returns versions", func() {
			var version1 json.RawMessage
			var version2 json.RawMessage

			BeforeEach(func() {
				version1 = json.RawMessage(`{"ref":"a"}`)
				version2 = json.RawMessage(`{"ref":"b"}`)

				checker.CheckResult = []*json.RawMessage{&version1, &version2}
			})

			It("responds with them", func() {
				var versions []*json.RawMessage
				err := json.NewDecoder(response.Body).Decode(&versions)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(versions).Should(Equal([]*json.RawMessage{&version1, &version2}))
			})
		})

		Context("when the check fails", func() {
			BeforeEach(func() {
				checker.CheckError = errors.New("oh no!")
			})

			It("returns 500", func() {
				Ω(response.StatusCode).Should(Equal(http.StatusInternalServerError))
			})
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
})
