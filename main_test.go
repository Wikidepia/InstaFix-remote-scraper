package main

import (
	"testing"
)

// TestHandleScrape tests handleScrape function
func TestHandleScrape(t *testing.T) {
	postID := "ClFHnYRsJk5"
	idata, err := handleScrape(postID)
	if err != nil {
		t.Errorf("parseGQL error: %v", err)
	}
	if !(idata.Username == "fatfatmillycat" && len(idata.Medias) == 5) {
		t.Errorf("handleScrape error: %v", idata)
	}
}
