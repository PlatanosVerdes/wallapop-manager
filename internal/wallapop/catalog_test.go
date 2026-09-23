package wallapop

import "testing"

// Shaped like a real answer: `expired` is present only on listings to reactivate.
const itemsFixture = `{
  "data": [
    {"id": "aaaaaaaaaaaa", "title": "Something on sale", "category_id": "12800",
     "created_date": 1780404091494, "modified_date": 1788421270977,
     "price": {"amount": 5.0, "currency": "EUR"}, "is_refurbished": {"flag": false}},
    {"id": "bbbbbbbbbbbb", "title": "Something expired", "category_id": "12800",
     "created_date": 1770404091494, "modified_date": 1778421270977,
     "price": {"amount": 49.0, "currency": "EUR"}, "expired": {"flag": true}},
    {"id": "cccccccccccc", "title": "Expired but flagged false", "category_id": "12800",
     "created_date": 1770404091494, "modified_date": 1778421270977,
     "price": {"amount": 20.0, "currency": "EUR"}, "expired": {"flag": false}}
  ],
  "meta": {}
}`

func TestDecodeItems(t *testing.T) {
	items, err := decodeItems([]byte(itemsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	if got := items[0].Price.Amount; got != 5 {
		t.Errorf("price came back as %v, expected 5", got)
	}
	if got := items[0].Price.Currency; got != "EUR" {
		t.Errorf("currency came back as %q", got)
	}

	want := []bool{false, true, false}
	for i, expected := range want {
		if items[i].NeedsReactivation() != expected {
			t.Errorf("item %s: NeedsReactivation() = %t, expected %t", items[i].ID, !expected, expected)
		}
	}
}

func TestDecodeItemsBareArray(t *testing.T) {
	items, err := decodeItems([]byte(`[{"id": "x", "title": "t", "expired": {"flag": true}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].NeedsReactivation() {
		t.Fatalf("a bare array did not decode: %+v", items)
	}
}

func TestDecodeItemsRubbish(t *testing.T) {
	if _, err := decodeItems([]byte(`"not an item list"`)); err == nil {
		t.Fatal("expected an error")
	}
}
