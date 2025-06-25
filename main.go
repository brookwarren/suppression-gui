package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	sesv2 "github.com/aws/aws-sdk-go-v2/service/sesv2"
)

// Server holds application state and AWS client
type Server struct {
	client *sesv2.Client

	listMu sync.RWMutex
	list   []string          // sorted list of original‑case addresses
	index  map[string]string // lowercase -> original case
}

func NewServer(ctx context.Context) (*Server, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	s := &Server{
		client: sesv2.NewFromConfig(cfg),
		index:  make(map[string]string),
	}
	// Populate list on startup (non‑fatal if it fails)
	if err := s.refresh(ctx); err != nil {
		log.Printf("initial refresh failed: %v", err)
	}
	return s, nil
}

// refresh queries AWS SES for *all* account-level suppressed addresses
func (s *Server) refresh(ctx context.Context) error {
	var (
		emails []string
		token  *string // pagination cursor
	)

	for {
		out, err := s.client.ListSuppressedDestinations(
			ctx,
			&sesv2.ListSuppressedDestinationsInput{
				NextToken: token,
				PageSize:  aws.Int32(1000),
			},
		)
		if err != nil {
			return err
		}

		for _, d := range out.SuppressedDestinationSummaries {
			emails = append(emails, aws.ToString(d.EmailAddress))
		}

		if out.NextToken == nil { // no more pages
			break
		}
		token = out.NextToken
	}

	// Case-insensitive sort A->Z
	sort.Slice(emails, func(i, j int) bool {
		return strings.ToLower(emails[i]) < strings.ToLower(emails[j])
	})

	// Build quick-lookup map
	idx := make(map[string]string, len(emails))
	for _, e := range emails {
		idx[strings.ToLower(e)] = e
	}

	s.listMu.Lock()
	s.list, s.index = emails, idx
	s.listMu.Unlock()
	return nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	s.listMu.RLock()
	out := append([]string(nil), s.list...)
	s.listMu.RUnlock()
	respondJSON(w, out)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := s.refresh(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.handleList(w, r)
}

type removeRequest struct {
	Emails string `json:"emails"`
}

type removeResponse struct {
	Results []string `json:"results"`
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req removeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Prepare list of trimmed addresses from user
	var inputs []string
	for _, line := range strings.Split(req.Emails, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			inputs = append(inputs, line)
		}
	}

	var results []string
	for _, input := range inputs {
		key := strings.ToLower(input)
		s.listMu.RLock()
		orig, ok := s.index[key]
		s.listMu.RUnlock()
		if !ok {
			results = append(results, "not found: "+input)
			continue
		}
		// Remove from AWS
		_, err := s.client.DeleteSuppressedDestination(r.Context(), &sesv2.DeleteSuppressedDestinationInput{
			EmailAddress: aws.String(orig),
		})
		if err != nil {
			results = append(results, "error: "+input+" ("+err.Error()+")")
			continue
		}
		// Update in‑memory cache
		s.listMu.Lock()
		delete(s.index, key)
		for i, v := range s.list {
			if v == orig {
				s.list = append(s.list[:i], s.list[i+1:]...)
				break
			}
		}
		s.listMu.Unlock()
		results = append(results, "removed: "+orig)
	}

	respondJSON(w, removeResponse{Results: results})
}

func respondJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Println("json encode error:", err)
	}
}

func main() {
	ctx := context.Background()
	srv, err := NewServer(ctx)
	if err != nil {
		log.Fatalf("failed to start: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlPage))
	})
	http.HandleFunc("/list", srv.handleList)
	http.HandleFunc("/update", srv.handleUpdate)
	http.HandleFunc("/remove", srv.handleRemove)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

const htmlPage = `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>SES Suppression List Manager</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 40px; }
        textarea { width: 100%; max-width: 600px; }
        button { margin-top: 8px; padding: 6px 16px; }
        h2 { margin-top: 24px; }
    </style>
</head>
<body>
    <h2>Suppression List</h2>
    <textarea id="suppressionList" rows="15" readonly></textarea><br>
    <button id="updateBtn">Update</button>

    <h2>Email Addresses to Remove</h2>
    <textarea id="removeInput" rows="10" placeholder="one address per line"></textarea><br>
    <button id="removeBtn">Remove</button>

    <script>
    async function loadList() {
        const res = await fetch('/list');
        const data = await res.json();
        document.getElementById('suppressionList').value = data.join('\n');
    }
    document.getElementById('updateBtn').onclick = async () => {
        const res = await fetch('/update', {method: 'POST'});
        const data = await res.json();
        document.getElementById('suppressionList').value = data.join('\n');
    };
    document.getElementById('removeBtn').onclick = async () => {
        const input = document.getElementById('removeInput').value;
        const res = await fetch('/remove', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({emails: input})
        });
        const data = await res.json();
        document.getElementById('removeInput').value = data.results.join('\n');
        await loadList();
    };
    window.onload = loadList;
    </script>
</body>
</html>`
