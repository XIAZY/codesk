package desktopurl

import "testing"

func TestValid(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:3000/w/product",
		"https://app.getcodesk.com/w/product",
	} {
		if !Valid(raw) {
			t.Errorf("Valid(%q) = false", raw)
		}
	}
	for _, raw := range []string{
		"",
		"/w/product",
		"ftp://app.getcodesk.com/w/product",
		"codesk://app.getcodesk.com/w/product",
		"https://user@app.getcodesk.com/w/product",
		"https://app.getcodesk.com/w/product?token=x",
		"https://app.getcodesk.com/w/product#",
		"https://app.getcodesk.com/w/product#fragment",
		" https://app.getcodesk.com/w/product",
		"https://app.getcodesk.com/w/\nproduct",
	} {
		if Valid(raw) {
			t.Errorf("Valid(%q) = true", raw)
		}
	}
}
