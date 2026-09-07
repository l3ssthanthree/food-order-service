package demorestaurant

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/avito-intern/kitchen/internal/domain"
)

const testOrderID = "20000000-0000-0000-0000-000000000001"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newTestServer(transport roundTripFunc) *Server {
	s := New(Config{
		KitchenBaseURL: "http://kitchen.test",
		PartnerToken:   "partner-secret",
		CallbackToken:  "callback-secret",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if transport != nil {
		s.client.Transport = transport
	}
	return s
}

func testOrder(status domain.OrderStatus, seconds int) domain.Order {
	created := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	return domain.Order{
		ID: testOrderID, Status: status, CreatedAt: created,
		UpdatedAt: created.Add(time.Duration(seconds) * time.Second),
	}
}

func responseJSON(status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}

func serveRequest(s *Server, method, path, token string, value any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(value)
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestOperatorRoutesRequirePartnerToken(t *testing.T) {
	s := newTestServer(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("unauthenticated operator must not reach Kitchen")
		return nil, nil
	})
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodGet, "/demo/v1/orders"},
		{http.MethodPost, "/demo/v1/orders/" + testOrderID + "/status"},
	} {
		for _, token := range []string{"", "wrong", "callback-secret"} {
			w := serveRequest(s, endpoint.method, endpoint.path, token, map[string]string{"status": "accepted"})
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s token %q: got %d %s", endpoint.path, token, w.Code, w.Body.String())
			}
		}
	}
	w := serveRequest(s, http.MethodGet, "/demo/v1/orders", "partner-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized list: %d %s", w.Code, w.Body.String())
	}
}

func TestEventRetriesAndLateCreationPreserveNewerOrder(t *testing.T) {
	s := newTestServer(nil)
	pending := testOrder(domain.OrderPendingRestaurant, 0)
	event := integrationEvent{EventID: uuid.NewString(), EventType: "order.created", OccurredAt: pending.CreatedAt, Order: pending}
	w := serveRequest(s, http.MethodPost, "/integration/v1/events", "callback-secret", event)
	if w.Code != http.StatusAccepted {
		t.Fatalf("initial event: %d %s", w.Code, w.Body.String())
	}
	s.orders[testOrderID] = testOrder(domain.OrderPreparing, 2)
	w = serveRequest(s, http.MethodPost, "/integration/v1/events", "callback-secret", event)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate event: %d %s", w.Code, w.Body.String())
	}
	event.EventID = uuid.NewString()
	w = serveRequest(s, http.MethodPost, "/integration/v1/events", "callback-secret", event)
	if w.Code != http.StatusAccepted || s.orders[testOrderID].Status != domain.OrderPreparing {
		t.Fatalf("late event regressed order: %d %+v", w.Code, s.orders[testOrderID])
	}
}

func TestOutOfOrderCreationDoesNotUndoCancellation(t *testing.T) {
	s := newTestServer(nil)
	for _, status := range []domain.OrderStatus{domain.OrderCancelled, domain.OrderPendingRestaurant} {
		order := testOrder(status, 0)
		eventType := "order.created"
		if status == domain.OrderCancelled {
			order = testOrder(status, 1)
			eventType = "order.cancelled"
		}
		event := integrationEvent{EventID: uuid.NewString(), EventType: eventType, OccurredAt: order.UpdatedAt, Order: order}
		w := serveRequest(s, http.MethodPost, "/integration/v1/events", "callback-secret", event)
		if w.Code != http.StatusAccepted {
			t.Fatalf("event: %d %s", w.Code, w.Body.String())
		}
	}
	if s.orders[testOrderID].Status != domain.OrderCancelled {
		t.Fatalf("cancelled order resurrected: %+v", s.orders[testOrderID])
	}
}

func TestEventAuthenticationAndPayloadValidation(t *testing.T) {
	s := newTestServer(nil)
	order := testOrder(domain.OrderPendingRestaurant, 0)
	event := integrationEvent{EventID: uuid.NewString(), EventType: "order.created", OccurredAt: order.UpdatedAt, Order: order}
	w := serveRequest(s, http.MethodPost, "/integration/v1/events", "partner-secret", event)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong callback token: %d", w.Code)
	}
	event.EventType = "order.cancelled"
	w = serveRequest(s, http.MethodPost, "/integration/v1/events", "callback-secret", event)
	if w.Code != http.StatusBadRequest || len(s.orders) != 0 || len(s.received) != 0 {
		t.Fatalf("invalid status accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestChangeStatusRecoversKnownOrderAndRetriesSameEvent(t *testing.T) {
	var postBodies [][]byte
	getCount := 0
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer partner-secret" {
			t.Fatalf("missing Kitchen partner auth: %v", r.Header)
		}
		if r.Method == http.MethodGet {
			getCount++
			return responseJSON(http.StatusOK, testOrder(domain.OrderPendingRestaurant, 0)), nil
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		postBodies = append(postBodies, body)
		if len(postBodies) == 1 {
			// Kitchen committed, but the first response was lost in transit.
			return nil, io.ErrUnexpectedEOF
		}
		return responseJSON(http.StatusOK, testOrder(domain.OrderAccepted, 1)), nil
	})
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusOK || s.orders[testOrderID].Status != domain.OrderAccepted {
		t.Fatalf("status recovery: %d %s", w.Code, w.Body.String())
	}
	if getCount != 1 || len(postBodies) != 2 || !bytes.Equal(postBodies[0], postBodies[1]) {
		t.Fatalf("retry must preserve exact payload: GET=%d POST=%q", getCount, postBodies)
	}
	var payload struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(postBodies[0], &payload); err != nil || uuid.Validate(payload.EventID) != nil {
		t.Fatalf("invalid event_id: %s", postBodies[0])
	}
}

func TestAlreadyAppliedStatusReturnsSuccessWithoutAnotherEvent(t *testing.T) {
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("already applied status must not emit another event")
		}
		return responseJSON(http.StatusOK, testOrder(domain.OrderAccepted, 1)), nil
	})
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusOK || s.orders[testOrderID].Status != domain.OrderAccepted {
		t.Fatalf("reconciliation: %d %s", w.Code, w.Body.String())
	}
}

func TestStaleLocalStateIsReconciledBeforeTransition(t *testing.T) {
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("cancelled order must not receive a status update")
		}
		return responseJSON(http.StatusOK, testOrder(domain.OrderCancelled, 1)), nil
	})
	s.orders[testOrderID] = testOrder(domain.OrderPendingRestaurant, 0)
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusConflict || s.orders[testOrderID].Status != domain.OrderCancelled {
		t.Fatalf("stale local state: %d %s", w.Code, w.Body.String())
	}
}

func TestLostStatusResponsesReconcileSuccessfulCommit(t *testing.T) {
	getCount, postCount := 0, 0
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			getCount++
			if getCount == 1 {
				return responseJSON(http.StatusOK, testOrder(domain.OrderPendingRestaurant, 0)), nil
			}
			return responseJSON(http.StatusOK, testOrder(domain.OrderAccepted, 1)), nil
		}
		postCount++
		return nil, io.ErrUnexpectedEOF
	})
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusOK || getCount != 2 || postCount != 3 {
		t.Fatalf("reconcile lost responses: %d GET=%d POST=%d %s", w.Code, getCount, postCount, w.Body.String())
	}
}

func TestKitchenConflictDoesNotRetryOrBecomeBadGateway(t *testing.T) {
	getCount, postCount := 0, 0
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			getCount++
			if getCount == 1 {
				return responseJSON(http.StatusOK, testOrder(domain.OrderPendingRestaurant, 0)), nil
			}
			return responseJSON(http.StatusOK, testOrder(domain.OrderCancelled, 1)), nil
		}
		postCount++
		return responseJSON(http.StatusConflict, map[string]string{"error": "invalid_status_transition"}), nil
	})
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusConflict || postCount != 1 || s.orders[testOrderID].Status != domain.OrderCancelled {
		t.Fatalf("Kitchen conflict: %d POST=%d %s", w.Code, postCount, w.Body.String())
	}
}

func TestUnknownPartnerOrderIsNotFound(t *testing.T) {
	s := newTestServer(func(_ *http.Request) (*http.Response, error) {
		return responseJSON(http.StatusNotFound, nil), nil
	})
	w := serveRequest(s, http.MethodPost, "/demo/v1/orders/"+testOrderID+"/status", "partner-secret", map[string]string{"status": "accepted"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown order: %d %s", w.Code, w.Body.String())
	}
}

func TestMenuSeedingIsCreateOnlyAndAcceptsExistingItems(t *testing.T) {
	seen := make(map[string]bool)
	s := newTestServer(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" ||
			r.Header.Get("Authorization") != "Bearer partner-secret" {
			t.Fatalf("unsafe seed request: %s %v", r.Method, r.Header)
		}
		externalID := strings.TrimPrefix(r.URL.Path, "/partner/v1/menu-items/")
		seen[externalID] = true
		return responseJSON(http.StatusPreconditionFailed, nil), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.SyncMenuUntilReady(ctx)
	if !s.ready.Load() || len(seen) != 3 {
		t.Fatalf("existing menu should be ready without writes: ready=%v items=%v", s.ready.Load(), seen)
	}
}
