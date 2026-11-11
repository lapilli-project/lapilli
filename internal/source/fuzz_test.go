package source

import (
	"os"
	"path/filepath"
	"testing"

	"lapilli.dev/internal/rules"
)

// What a ruler answers is read, whatever it is: answered or refused, never
// a panic (docs/design/rulers.md). Seeded with the answers Mimir 3.2.4
// gave and those Amazon's and Azure's API documents show.

func seed(f *testing.F, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
}

func FuzzConfigDocs(f *testing.F) {
	seed(f, "mimir-3.2.4-list.yaml")
	f.Add([]byte("{}\n"))
	f.Add([]byte("a: [{name: g, rules: [{alert: A, expr: up == 0, for: 5m}]}]\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		docs, err := ConfigDocs("http://mimir/rules", b)
		if err != nil {
			return
		}
		rules.FromDocs(docs)
	})
}

func FuzzAmazon(f *testing.F) {
	seed(f, "amazon-list.json")
	seed(f, "amazon-describe.json")
	f.Fuzz(func(t *testing.T, b []byte) {
		AmazonList(b)
		if _, data, err := AmazonDescribed(b); err == nil {
			rules.Parse(data)
		}
	})
}

func FuzzAzure(f *testing.F) {
	seed(f, "azure-list.json")
	f.Fuzz(func(t *testing.T, b []byte) {
		items, _, err := AzureList(b)
		if err != nil {
			return
		}
		r := &Ruler{URL: "https://management.azure.com/subscriptions/sub-1", Kind: RulerAzure}
		for _, it := range items {
			if d, ok, err := r.azureDoc(it); err == nil && ok {
				rules.Parse(d.Data)
				AzureHash(d.Data)
				rules.Shape(d.Data, AzureSets)
			}
		}
	})
}
