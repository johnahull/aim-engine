// MIT License
//
// Copyright (c) 2025 Advanced Micro Devices, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package aimprofile

import (
	"sort"
	"testing"
)

func TestCompareProfileVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  string
		right string
		want  int
	}{
		{name: "semantic minor ordering", left: "0.10.0", right: "0.9.0", want: 1},
		{name: "calendar month ordering", left: "2026.10.0-preview", right: "2026.9.0-preview", want: 1},
		{name: "calendar preview sorts before rc", left: "2026.8.0-preview", right: "2026.8.0-rc1", want: -1},
		{name: "calendar rc sorts before full release", left: "2026.8.0-rc1", right: "2026.8.0", want: -1},
		{name: "stable beats preview", left: "2026.8.0", right: "2026.8.0-preview", want: 1},
		{name: "semantic version beats arbitrary tag", left: "2026.8.0-preview", right: "nightly", want: 1},
		{name: "natural fallback for arbitrary tags", left: "release-10", right: "release-9", want: 1},
		{name: "empty ranks lowest", left: "", right: "2026.8.0-preview", want: -1},
		{name: "equal", left: "2026.8.0-preview", right: "2026.8.0-preview", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := compareProfileVersions(tt.left, tt.right); got != tt.want {
				t.Fatalf("compareProfileVersions(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

func TestCompareProfileVersionsCalendarReleaseOrder(t *testing.T) {
	t.Parallel()

	versions := []string{
		"2026.8.0-preview",
		"2026.10.0-preview",
		"2026.8.0",
		"2026.9.0",
		"2026.8.0-rc1",
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareProfileVersions(versions[i], versions[j]) > 0
	})

	want := []string{
		"2026.10.0-preview",
		"2026.9.0",
		"2026.8.0",
		"2026.8.0-rc1",
		"2026.8.0-preview",
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("versions = %v, want %v", versions, want)
		}
	}
}
