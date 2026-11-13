package postal

import "testing"

func TestAddressesAreCleanedAndWrittenTheUSPSWay(t *testing.T) {
	a, err := Address{Name: "  Pat  Debtor ", Line1: "22 Oak Ave", Line2: "Apt 4", City: "Mesa", State: "az", Zip: "85201-1234"}.Clean()
	if err != nil || a.Name != "Pat Debtor" || a.State != "AZ" {
		t.Fatalf("%+v %v", a, err)
	}
	if got := a.OneLine(); got != "22 OAK AVE APT 4, MESA AZ 85201-1234" {
		t.Fatal(got)
	}
	for _, bad := range []Address{
		{Name: "Pat", Line1: "1 A St", City: "Mesa", State: "Arizona", Zip: "85201"},
		{Name: "Pat", Line1: "1 A St", City: "Mesa", State: "AZ", Zip: "852"},
		{Line1: "1 A St", City: "Mesa", State: "AZ", Zip: "85201"},
	} {
		if _, err := bad.Clean(); err != ErrAddress {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestALettersCostCountsSheetsAndExtraServices(t *testing.T) {
	plain := Options{}.Estimate(3)
	if plain.Sheets != 4 || plain.CostCents != priceLetter+3*priceExtraSheet {
		t.Fatalf("%+v", plain)
	}
	both := Options{DoubleSided: true}.Estimate(3)
	if both.Sheets != 3 {
		t.Fatalf("%+v", both)
	}
	certified := Options{ExtraService: "certified_return_receipt"}.Estimate(1)
	if certified.CostCents != priceLetter+priceExtraSheet+priceCertified+priceReturnReceipt {
		t.Fatalf("%+v", certified)
	}
	if (Options{ExtraService: "registered"}).Check() != ErrOptions {
		t.Fatal("registered mail is not offered")
	}
}

func TestPagesCountsPageObjectsButNotThePageTree(t *testing.T) {
	if n := Pages([]byte("%PDF-1.4 << /Type /Page >> << /Type/Page >> << /Type /Pages >>")); n != 2 {
		t.Fatal(n)
	}
	if n := Pages([]byte("%PDF-1.7 compressed")); n != 1 {
		t.Fatal(n)
	}
}
