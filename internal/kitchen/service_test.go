package kitchen

import (
	"errors"
	"strings"
	"testing"

	"github.com/avito-intern/kitchen/internal/domain"
)

func TestNormalizeItemsMergesDuplicatesAndSorts(t *testing.T) {
	t.Parallel()
	first := "00000000-0000-0000-0000-000000000001"
	second := "00000000-0000-0000-0000-000000000002"
	got, err := normalizeItems([]domain.CreateOrderItem{
		{MenuItemID: second, Quantity: 2},
		{MenuItemID: first, Quantity: 1},
		{MenuItemID: second, Quantity: 3},
	})
	if err != nil {
		t.Fatalf("normalizeItems returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	if got[0].MenuItemID != first || got[0].Quantity != 1 {
		t.Fatalf("unexpected first line: %+v", got[0])
	}
	if got[1].MenuItemID != second || got[1].Quantity != 5 {
		t.Fatalf("unexpected second line: %+v", got[1])
	}
}

func TestNormalizeItemsRejectsInvalidQuantity(t *testing.T) {
	t.Parallel()
	_, err := normalizeItems([]domain.CreateOrderItem{{
		MenuItemID: "00000000-0000-0000-0000-000000000001",
		Quantity:   0,
	}})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("error = %v, want ErrInvalidInput", err)
	}
}

func TestNormalizeItemsCanonicalizesEquivalentUUIDs(t *testing.T) {
	id := "aabbccdd-0000-0000-0000-000000000001"
	items, err := normalizeItems([]domain.CreateOrderItem{
		{MenuItemID: id, Quantity: 1},
		{MenuItemID: strings.ToUpper(id), Quantity: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Quantity != 3 || items[0].MenuItemID != id {
		t.Fatalf("equivalent IDs must share one reservation: %+v", items)
	}
}
