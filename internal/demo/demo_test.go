// Copyright 2026 Autometiq
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package demo

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestProgressLineFitsBesideASpinner(t *testing.T) {
	got := progressLine("a1b2c3d4e5f6: Downloading [====>            ]  12.3MB/45.6MB")
	if strings.HasPrefix(got, "a1b2") {
		t.Errorf("layer id not dropped: %q", got)
	}
	if len(got) > 48 {
		t.Errorf("line too long to redraw in place: %d chars", len(got))
	}
	// A line with no layer id, and a colon too far in to be one, is left alone.
	if got := progressLine("  Status: Downloaded newer image  "); got != "Status: Downloaded newer image" {
		t.Errorf("got %q", got)
	}
}

// TestStartIsIdempotent is the check that matters: the demo has to survive
// being run twice, including after a first attempt was interrupted. It needs a
// real Docker, so it is opt-in -- it creates the demo container and leaves it.
func TestStartIsIdempotent(t *testing.T) {
	if os.Getenv("SAFESLICE_TEST_DOCKER") == "" {
		t.Skip("set SAFESLICE_TEST_DOCKER=1 to run the demo container tests")
	}
	ctx := context.Background()
	if err := DockerAvailable(ctx); err != nil {
		t.Skipf("docker not usable: %v", err)
	}

	for _, pass := range []string{"first", "second"} {
		if err := Start(ctx, nil); err != nil {
			t.Fatalf("%s Start: %v", pass, err)
		}
		if !Running(ctx) {
			t.Fatalf("%s Start returned but the container is not running", pass)
		}
		if !seeded(ctx) {
			t.Fatalf("%s Start returned but the source is not seeded", pass)
		}
		if counts, err := Counts(ctx); err != nil || counts == "" {
			t.Fatalf("%s Counts: %q %v", pass, counts, err)
		}
		// The target has to exist for anything to be loaded into it.
		if err := ResetTarget(ctx); err != nil {
			t.Fatalf("%s ResetTarget: %v", pass, err)
		}
	}

	// A dropped target is repaired rather than reported as ready.
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres",
		"-c", "DROP DATABASE "+TargetDB); err != nil {
		t.Fatalf("dropping the target: %v", err)
	}
	if err := Start(ctx, nil); err != nil {
		t.Fatalf("Start after dropping the target: %v", err)
	}
	if err := ResetTarget(ctx); err != nil {
		t.Fatalf("target was not recreated: %v", err)
	}
}
