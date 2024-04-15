package uhc_test

import (
	"net/http"
	"testing"

	"github.com/zijiren233/go-uhc"
)

func TestOpenAI(t *testing.T) {
	UA := `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36 Edg/123.0.0.0`
	req, _ := http.NewRequest(http.MethodGet, "https://chat.openai.com", nil)
	req.Header.Set("User-Agent", UA)
	resp, err := uhc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}
	t.Logf("uhc status code: %d", resp.StatusCode)

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status code: %d", resp2.StatusCode)
	}
	t.Logf("http default client status code: %d", resp2.StatusCode)
}
