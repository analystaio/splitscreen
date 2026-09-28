package wake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type fakeEC2 struct {
	calls []string
	prev  types.InstanceStateName
	err   error
}

func (f *fakeEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.calls = append(f.calls, in.InstanceIds...)
	if f.err != nil {
		return nil, f.err
	}
	id := in.InstanceIds[0]
	return &ec2.StartInstancesOutput{StartingInstances: []types.InstanceStateChange{{
		InstanceId:    &id,
		PreviousState: &types.InstanceState{Name: f.prev},
		CurrentState:  &types.InstanceState{Name: types.InstanceStateNamePending},
	}}}, nil
}

func newFake(f *fakeEC2, regions *[]string) *EC2 {
	e := NewEC2("us-east-2")
	e.newClient = func(_ context.Context, region string) (startAPI, error) {
		*regions = append(*regions, region)
		return f, nil
	}
	return e
}

func TestStartReportsTransition(t *testing.T) {
	f := &fakeEC2{prev: types.InstanceStateNameStopped}
	var regions []string
	e := newFake(f, &regions)

	res, err := e.Start(context.Background(), "", "i-0123456789abcdef0")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Started() || res.CurrentState != "pending" {
		t.Fatalf("result = %+v, want a stopped->pending start", res)
	}
	if _, err := e.Start(context.Background(), "ca-central-1", "i-0123456789abcdef1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), "", "i-0123456789abcdef0"); err != nil {
		t.Fatal(err)
	}
	// An empty region uses the default, and clients are cached per region.
	if strings.Join(regions, ",") != "us-east-2,ca-central-1" {
		t.Errorf("clients built for %v", regions)
	}
	if len(f.calls) != 3 {
		t.Errorf("StartInstances calls = %d, want 3", len(f.calls))
	}
}

func TestStartAlreadyRunningIsNotAStart(t *testing.T) {
	var regions []string
	e := newFake(&fakeEC2{prev: types.InstanceStateNameRunning}, &regions)
	res, err := e.Start(context.Background(), "", "i-0123456789abcdef0")
	if err != nil || res.Started() {
		t.Fatalf("res=%+v err=%v; a running instance was not started by this call", res, err)
	}
}

func TestStartClassifiesErrors(t *testing.T) {
	cases := []struct {
		code string
		want string
		is   error
	}{
		{"IncorrectInstanceState", "", ErrStopping},
		{"UnauthorizedOperation", "not allowed", nil},
		{"InvalidInstanceID.NotFound", "does not exist", nil},
		{"SomethingElse", "boom", nil},
	}
	for _, c := range cases {
		var regions []string
		e := newFake(&fakeEC2{err: &smithy.GenericAPIError{Code: c.code, Message: "boom"}}, &regions)
		_, err := e.Start(context.Background(), "", "i-0123456789abcdef0")
		if err == nil {
			t.Fatalf("%s: expected an error", c.code)
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%s: err = %v, want %v", c.code, err, c.is)
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.code, err, c.want)
		}
	}
}
