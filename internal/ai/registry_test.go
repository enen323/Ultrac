package ai

import (
	"context"
	"errors"
	"testing"
)

// fakeProvider records how it was called and returns a fixed stream.
func fakeProvider(name string) StreamFn {
	return func(ctx context.Context, model Model, tc TranscriptContext, opts StreamOptions) Stream {
		return StreamOf(
			EvStart{},
			EvTextDelta{ContentIndex: 0, Delta: "from " + name},
			EvDone{Reason: StopStop, Message: NewTextMessage(RoleAssistant, "from "+name)},
		)
	}
}

func TestRegisterResolveAndDuplicatePanic(t *testing.T) {
	// Registration is process-global, so this test uses an api dialect no
	// adapter registers on its own.
	const testApi Api = "test-registry-api"

	Register(ApiProvider{API: testApi, Stream: fakeProvider("registered")})
	if !Registered(testApi) {
		t.Fatal("Registered reports false after Register")
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("a duplicate registration must panic, not silently replace")
		}
	}()
	Register(ApiProvider{API: testApi, Stream: fakeProvider("duplicate")})
}

func TestRegisterRejectsBadInput(t *testing.T) {
	for name, p := range map[string]ApiProvider{
		"empty api":  {Stream: fakeProvider("x")},
		"nil stream": {API: "test-nil-stream"},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Register(%s) must panic", name)
				}
			}()
			Register(p)
		})
	}
}

func TestStreamRequestResolvesByApi(t *testing.T) {
	const testApi Api = "test-resolve-api"
	Register(ApiProvider{API: testApi, Stream: fakeProvider("resolved")})

	model := Model{ID: "m", API: testApi, Provider: "test"}
	s, err := StreamRequest(context.Background(), model, TranscriptContext{}, StreamOptions{})
	if err != nil {
		t.Fatalf("StreamRequest: %v", err)
	}
	msg, err := s.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if MessageText(msg) != "from resolved" {
		t.Errorf("text = %q", MessageText(msg))
	}
}

func TestStreamRequestUnknownApiIsAnErrorNotAPanic(t *testing.T) {
	model := Model{ID: "m", API: "test-unregistered-api", Provider: "test"}
	s, err := StreamRequest(context.Background(), model, TranscriptContext{}, StreamOptions{})
	if err == nil {
		t.Fatal("an unregistered api must be an error")
	}
	if s != nil {
		t.Error("no stream may accompany the error")
	}
	var unknown *UnknownAPIError
	if !errors.As(err, &unknown) {
		t.Errorf("error type = %T, want *UnknownAPIError", err)
	}
	if unknown.API != model.API {
		t.Errorf("error api = %q", unknown.API)
	}
}

func TestApisListsRegistered(t *testing.T) {
	const testApi Api = "test-list-api"
	Register(ApiProvider{API: testApi, Stream: fakeProvider("listed")})

	found := false
	for _, a := range Apis() {
		if a == testApi {
			found = true
		}
	}
	if !found {
		t.Errorf("Apis() = %v, missing the test dialect", Apis())
	}
}
