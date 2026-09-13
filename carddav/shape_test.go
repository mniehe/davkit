package carddav_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/mniehe/davkit/carddav"
	"github.com/mniehe/davkit/carddavmem"
)

func TestPutRefusesACardThatIsNotUTF8(t *testing.T) {
	h := handlerFor(t, newStore(t), carddav.Config{})

	w := put(h, "/alice/work/latin1.vcf", "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:latin1\r\nFN:Jos\xe9\r\nEND:VCARD\r\n", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !strings.Contains(w.Body.String(), "valid-address-data") {
		t.Errorf("body = %q, want the CARDDAV:valid-address-data precondition", w.Body.String())
	}
}

// refusesContent stands for a backend whose own validation is stricter than
// the library's.
type refusesContent struct{ *carddavmem.Store }

func (refusesContent) CompareAndStoreItem(context.Context, carddav.ItemRef, carddav.StoreItemRequest) (carddav.StoreItemResult, error) {
	return carddav.StoreItemResult{}, &carddav.InvalidContentError{Err: errors.New("the backend's own rule")}
}

func TestPutAnswersABackendContentRefusalAsInvalidData(t *testing.T) {
	h := handlerFor(t, refusesContent{newStore(t)}, carddav.Config{})

	w := put(h, "/alice/work/review.vcf", reviewVCF, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d\n%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "valid-address-data") {
		t.Errorf("body = %q, want the CARDDAV:valid-address-data precondition", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "the backend's own rule") {
		t.Errorf("body = %q leaks the backend's reason", w.Body.String())
	}
}
