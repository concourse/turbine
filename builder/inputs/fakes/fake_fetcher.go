// This file was generated by counterfeiter
package fakes

import (
	"sync"

	"github.com/concourse/turbine"
	"github.com/concourse/turbine/builder/inputs"
	"github.com/concourse/turbine/event"
)

type FakeFetcher struct {
	FetchStub        func([]turbine.Input, event.Emitter, <-chan struct{}) ([]inputs.FetchedInput, error)
	fetchMutex       sync.RWMutex
	fetchArgsForCall []struct {
		arg1 []turbine.Input
		arg2 event.Emitter
		arg3 <-chan struct{}
	}
	fetchReturns struct {
		result1 []inputs.FetchedInput
		result2 error
	}
}

func (fake *FakeFetcher) Fetch(arg1 []turbine.Input, arg2 event.Emitter, arg3 <-chan struct{}) ([]inputs.FetchedInput, error) {
	fake.fetchMutex.Lock()
	fake.fetchArgsForCall = append(fake.fetchArgsForCall, struct {
		arg1 []turbine.Input
		arg2 event.Emitter
		arg3 <-chan struct{}
	}{arg1, arg2, arg3})
	fake.fetchMutex.Unlock()
	if fake.FetchStub != nil {
		return fake.FetchStub(arg1, arg2, arg3)
	} else {
		return fake.fetchReturns.result1, fake.fetchReturns.result2
	}
}

func (fake *FakeFetcher) FetchCallCount() int {
	fake.fetchMutex.RLock()
	defer fake.fetchMutex.RUnlock()
	return len(fake.fetchArgsForCall)
}

func (fake *FakeFetcher) FetchArgsForCall(i int) ([]turbine.Input, event.Emitter, <-chan struct{}) {
	fake.fetchMutex.RLock()
	defer fake.fetchMutex.RUnlock()
	return fake.fetchArgsForCall[i].arg1, fake.fetchArgsForCall[i].arg2, fake.fetchArgsForCall[i].arg3
}

func (fake *FakeFetcher) FetchReturns(result1 []inputs.FetchedInput, result2 error) {
	fake.FetchStub = nil
	fake.fetchReturns = struct {
		result1 []inputs.FetchedInput
		result2 error
	}{result1, result2}
}

var _ inputs.Fetcher = new(FakeFetcher)
