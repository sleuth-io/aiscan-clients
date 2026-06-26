package capture

import (
	"context"
	"errors"
	"testing"
)

// recipe is a small helper for building test recipes.
func recipe(id SourceID, detect bool, arts []Artifact, err error) Recipe {
	return Recipe{
		ID:     id,
		Detect: func() bool { return detect },
		Capture: func(ctx context.Context, opts Options) ([]Artifact, error) {
			return arts, err
		},
	}
}

func TestRunConcatenatesAvailableSources(t *testing.T) {
	a := recipe("a", true, []Artifact{{Source: "a"}, {Source: "a"}}, nil)
	b := recipe("b", true, []Artifact{{Source: "b"}}, nil)
	arts, errs := Run(context.Background(), []Recipe{a, b}, Options{})
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(arts) != 3 {
		t.Fatalf("got %d artifacts, want 3", len(arts))
	}
}

func TestRunSkipsUndetectedSources(t *testing.T) {
	off := recipe("off", false, []Artifact{{Source: "off"}}, nil)
	on := recipe("on", true, []Artifact{{Source: "on"}}, nil)
	arts, _ := Run(context.Background(), []Recipe{off, on}, Options{})
	if len(arts) != 1 || arts[0].Source != "on" {
		t.Fatalf("got %v, want only the detected source", arts)
	}
}

func TestRunIsolatesFailingSource(t *testing.T) {
	bad := recipe("bad", true, nil, errors.New("boom"))
	good := recipe("good", true, []Artifact{{Source: "good"}}, nil)
	arts, errs := Run(context.Background(), []Recipe{bad, good}, Options{})
	// A failing source must not block the others.
	if len(arts) != 1 || arts[0].Source != "good" {
		t.Fatalf("got %v, want the good source to still run", arts)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly 1", errs)
	}
}

func TestRunNilDetectAlwaysRuns(t *testing.T) {
	r := Recipe{
		ID:      "always",
		Detect:  nil, // no detector → treated as available
		Capture: func(ctx context.Context, o Options) ([]Artifact, error) { return []Artifact{{Source: "always"}}, nil },
	}
	arts, _ := Run(context.Background(), []Recipe{r}, Options{})
	if len(arts) != 1 {
		t.Fatalf("got %d, want 1 (nil Detect should run)", len(arts))
	}
}
