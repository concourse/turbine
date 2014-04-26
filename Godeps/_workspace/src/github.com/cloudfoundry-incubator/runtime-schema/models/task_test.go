package models_test

import (
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	. "github.com/cloudfoundry-incubator/runtime-schema/models"
)

var _ = Describe("Task", func() {
	var task Task

	taskPayload := `{
		"guid":"some-guid",
		"reply_to":"some-requester",
		"stack":"some-stack",
		"executor_id":"executor",
		"actions":[
			{
				"action":"download",
				"args":{"from":"old_location","to":"new_location","extract":true}
			}
		],
		"container_handle":"17fgsafdfcvc",
		"result": "turboencabulated",
		"failed":true,
		"failure_reason":"because i said so",
		"file_descriptors":9001,
		"memory_mb":256,
		"disk_mb":1024,
		"cpu_percent": 42.25,
		"log": {
			"guid": "123",
			"source_name": "APP",
			"index": 42
		},
		"created_at": 1393371971000000000,
		"updated_at": 1393371971000000010,
		"state": 1
	}`

	BeforeEach(func() {
		index := 42

		task = Task{
			Guid:    "some-guid",
			ReplyTo: "some-requester",
			Stack:   "some-stack",
			Actions: []ExecutorAction{
				{
					Action: DownloadAction{
						From:    "old_location",
						To:      "new_location",
						Extract: true,
					},
				},
			},
			Log: LogConfig{
				Guid:       "123",
				SourceName: "APP",
				Index:      &index,
			},
			ExecutorID:      "executor",
			ContainerHandle: "17fgsafdfcvc",
			Result:          "turboencabulated",
			Failed:          true,
			FailureReason:   "because i said so",
			FileDescriptors: 9001,
			MemoryMB:        256,
			DiskMB:          1024,
			CpuPercent:      42.25,
			CreatedAt:       time.Date(2014, time.February, 25, 23, 46, 11, 00, time.UTC).UnixNano(),
			UpdatedAt:       time.Date(2014, time.February, 25, 23, 46, 11, 10, time.UTC).UnixNano(),
			State:           TaskStatePending,
		}
	})

	Describe("ToJSON", func() {
		It("should JSONify", func() {
			json := task.ToJSON()
			Ω(string(json)).Should(MatchJSON(taskPayload))
		})
	})

	Describe("NewTaskFromJSON", func() {
		It("returns a Task with correct fields", func() {
			decodedTask, err := NewTaskFromJSON([]byte(taskPayload))
			Ω(err).ShouldNot(HaveOccurred())

			Ω(decodedTask).Should(Equal(task))
		})

		Context("with an invalid payload", func() {
			It("returns the error", func() {
				decodedTask, err := NewTaskFromJSON([]byte("butts lol"))
				Ω(err).Should(HaveOccurred())

				Ω(decodedTask).Should(BeZero())
			})
		})
	})
})
