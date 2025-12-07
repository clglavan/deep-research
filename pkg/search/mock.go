package search

import "fmt"

type MockClient struct{}

func (m *MockClient) Search(query string) ([]Result, error) {
	return m.SearchWithPage(query, 1)
}

func (m *MockClient) SearchWithPage(query string, page int) ([]Result, error) {
	return []Result{
		{
			Title:   fmt.Sprintf("Mock Result for %s (page %d)", query, page),
			URL:     fmt.Sprintf("http://example.com/page%d", page),
			Content: fmt.Sprintf("This is some mock content found for the query '%s' on page %d. It contains some facts.", query, page),
		},
	}, nil
}
