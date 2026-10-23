package video

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shotwise/shotwise/internal/config"
	"github.com/shotwise/shotwise/internal/domain/project"
	"github.com/shotwise/shotwise/internal/provider"
)

type fakeVideoCaps struct {
	view config.VideoCapabilityView
	err  error
}

func (f *fakeVideoCaps) VideoCapabilitiesForProject(context.Context, *project.ProjectFile) (config.VideoCapabilityView, error) {
	return f.view, f.err
}

func TestGetVideoCapabilitiesReturnsModelFacts(t *testing.T) {
	projects := &fakeAssetProjects{pf: &project.ProjectFile{ContentMode: "drama", GenerationMode: "storyboard"}}
	reader := &fakeVideoCaps{view: config.VideoCapabilityView{
		ProviderID:         "p1",
		ModelID:            "m1",
		Source:             "registry",
		Caps:               provider.VideoCapabilities{FirstFrame: true, LastFrame: true, MaxReferenceImages: 3},
		SupportedDurations: []int{4, 8, 12},
	}}
	tl, err := NewGetVideoCapabilitiesTool("proj", reader, projects)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope: %v: %s", err, out)
	}
	if env.IsError {
		t.Fatalf("unexpected error envelope: %s", out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(env.Content[0].Text), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["model"] != "m1" || payload["max_duration"].(float64) != 12 ||
		payload["max_reference_images"].(float64) != 3 || payload["first_frame"] != true {
		t.Fatalf("payload mismatch: %v", payload)
	}
	if payload["default_duration"] != nil {
		t.Fatalf("unset default_duration must be null: %v", payload["default_duration"])
	}
}

func TestGetVideoCapabilitiesDeniesUnresolvableOrUndeclaredModel(t *testing.T) {
	projects := &fakeAssetProjects{pf: &project.ProjectFile{GenerationMode: "reference_video"}}

	failing := &fakeVideoCaps{err: errors.New("bucket r2v missing")}
	tl, _ := NewGetVideoCapabilitiesTool("proj", failing, projects)
	out, _ := tl.InvokableRun(context.Background(), `{}`)
	if !strings.Contains(out, "bucket r2v missing") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("resolver error must surface as is_error: %s", out)
	}

	undeclared := &fakeVideoCaps{view: config.VideoCapabilityView{ProviderID: "p", ModelID: "m"}}
	tl, _ = NewGetVideoCapabilitiesTool("proj", undeclared, projects)
	out, _ = tl.InvokableRun(context.Background(), `{}`)
	if !strings.Contains(out, "未声明 supported_durations") || !strings.Contains(out, `"is_error":true`) {
		t.Fatalf("model without durations must fail loud: %s", out)
	}
}
