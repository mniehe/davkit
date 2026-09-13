package caldav

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// shapeEvent is a calendar whose one VEVENT carries lines; VCALENDAR and
// VEVENT are its first two components and levels.
func shapeEvent(lines ...string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:shape\r\n" +
		strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}

func nestedLevels(levels int) []byte {
	lines := make([]string, 0, 2*levels)
	for i := range levels {
		lines = append(lines, fmt.Sprintf("BEGIN:X-%d", i))
	}
	for i := levels - 1; i >= 0; i-- {
		lines = append(lines, fmt.Sprintf("END:X-%d", i))
	}
	return shapeEvent(lines...)
}

func componentCount(total int) []byte {
	lines := make([]string, 0, 2*(total-2))
	for range total - 2 {
		lines = append(lines, "BEGIN:VALARM", "END:VALARM")
	}
	return shapeEvent(lines...)
}

func TestCheckShapeHoldsEachBoundExactly(t *testing.T) {
	atLimit := strings.Repeat("a", maxParamValueBytes)

	tests := []struct {
		name    string
		body    []byte
		wantErr error
	}{
		{"deepest allowed", nestedLevels(maxComponentDepth - 2), nil},
		{"one level deeper", nestedLevels(maxComponentDepth - 1), errTooDeep},
		{"most components allowed", componentCount(maxComponents), nil},
		{"one component more", componentCount(maxComponents + 1), errTooManyComponents},
		{"longest parameter value", shapeEvent("X-A;X-P=" + atLimit + ":v"), nil},
		{"one byte longer", shapeEvent("X-A;X-P=" + atLimit + "a:v"), errLongParamValue},
		{"longest quoted value", shapeEvent(`X-A;X-P="` + atLimit + `":v`), nil},
		{"quoted, one byte longer", shapeEvent(`X-A;X-P="` + atLimit + `a":v`), errLongParamValue},
		{"a long value in a list", shapeEvent("X-A;X-P=a," + atLimit + "a:v"), errLongParamValue},
		{"a long value in a second parameter", shapeEvent("X-A;X-Q=1;X-P=" + atLimit + "a:v"), errLongParamValue},
		{"a quoted value holding a colon", shapeEvent(`X-A;X-P="a:b";X-Q=c:v`), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckShape(tt.body)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckShape = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				return
			}

			var invalid *InvalidContentError
			if !errors.As(err, &invalid) {
				t.Errorf("CheckShape = %T, want an *InvalidContentError a backend can return as is", err)
			}
			if strings.Contains(err.Error(), "aaaa") || strings.Contains(err.Error(), "X-") {
				t.Errorf("error %q quotes the body", err)
			}
		})
	}
}
