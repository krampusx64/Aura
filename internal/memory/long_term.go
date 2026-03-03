package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aurago/internal/config"

	chromem "github.com/philippgille/chromem-go"
)

// ArchiveItem represents a single concept/content pair for batch archiving.
type ArchiveItem struct {
	Concept string `json:"concept"`
	Content string `json:"content"`
	Domain  string `json:"domain,omitempty"` // Phase C: optional domain tag for cross-domain learning
}

// VectorDB represents a generic vector database for long term storage
type VectorDB interface {
	StoreDocument(concept, content string) ([]string, error)
	StoreBatch(items []ArchiveItem) ([]string, error)
	SearchSimilar(query string, topK int) ([]string, []string, error)
	GetByID(id string) (string, error)
	DeleteDocument(id string) error
	Close() error
}

// ChromemVectorDB implements VectorDB using chromem-go with persistence.
type ChromemVectorDB struct {
	db            *chromem.DB
	collection    *chromem.Collection
	logger        *slog.Logger
	mu            sync.Mutex // Thread-safe writes
	embeddingFunc chromem.EmbeddingFunc
}

func (cv *ChromemVectorDB) Close() error {
	// Chromem-go's persistent DB doesn't have an explicit Close() method in current versions,
	// but we implement it to satisfy the interface and allow for future cleanup.
	cv.logger.Info("Closing VectorDB (no-op for chromem)")
	return nil
}

// NewChromemVectorDB creates a new persistent Vector DB backed by chromem-go.
// It selects the embedding function based on the config:
//   - "internal": uses the main LLM provider's API (e.g., OpenRouter) for embeddings
//   - "external": uses a dedicated embedding endpoint (e.g., local Ollama)
func NewChromemVectorDB(cfg *config.Config, logger *slog.Logger) (*ChromemVectorDB, error) {
	db, err := chromem.NewPersistentDB(cfg.Directories.VectorDBDir, false)
	if err != nil {
		return nil, fmt.Errorf("failed to create persistent vector DB: %w", err)
	}

	// Dynamic embedding function factory using chromem-go's native constructors
	var embeddingFunc chromem.EmbeddingFunc
	provider := cfg.Embeddings.Provider

	switch provider {
	case "internal":
		// Use the main LLM provider's API for embeddings (OpenRouter, etc.)
		embedModel := cfg.Embeddings.InternalModel
		if embedModel == "" {
			embedModel = "text-embedding-3-small" // Default fallback
		}
		embeddingFunc = chromem.NewEmbeddingFuncOpenAICompat(
			cfg.LLM.BaseURL,
			cfg.LLM.APIKey,
			embedModel,
			nil, // Auto-detect normalization
		)
		logger.Info("VectorDB using internal embeddings via LLM provider", "url", cfg.LLM.BaseURL, "model", embedModel)
	case "external":
		// Use a dedicated embedding endpoint (Ollama, local server, etc.)
		embeddingFunc = chromem.NewEmbeddingFuncOpenAICompat(
			cfg.Embeddings.ExternalURL,
			cfg.Embeddings.APIKey,
			cfg.Embeddings.ExternalModel,
			nil, // Auto-detect normalization
		)
		logger.Info("VectorDB using external embeddings", "url", cfg.Embeddings.ExternalURL, "model", cfg.Embeddings.ExternalModel)
	default:
		return nil, fmt.Errorf("unknown embeddings provider: %q (must be 'internal' or 'external')", provider)
	}

	collection, err := db.GetOrCreateCollection("aurago_memories", nil, embeddingFunc)
	if err != nil {
		return nil, fmt.Errorf("failed to get/create collection: %w", err)
	}

	vdb := &ChromemVectorDB{
		db:            db,
		collection:    collection,
		logger:        logger,
		embeddingFunc: embeddingFunc,
	}

	// Phase 29: Startup validation — test the embedding pipeline
	logger.Info("Validating embedding pipeline (60s timeout)...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vec, err := embeddingFunc(ctx, "startup validation test")
	if err != nil {
		logger.Warn("Embedding pipeline validation failed. Long-term memory will be disabled.", "error", err)
		// We return the DB instance anyway but future calls might fail or be ignored.
		// For now, chromem-go will just error on Store/Search if the function keeps failing.
	} else {
		logger.Info("Embedding pipeline validated", "vector_dimensions", len(vec), "provider", provider, "docs", collection.Count())
	}

	return vdb, nil
}

// StoreDocument stores a concept/content pair, auto-chunking large texts.
// Returns the list of stored document IDs.
func (cv *ChromemVectorDB) StoreDocument(concept, content string) ([]string, error) {
	return cv.StoreDocumentWithDomain(concept, content, "")
}

// StoreDocumentWithDomain stores a concept/content pair with an optional domain tag
// for cross-domain learning (Phase C). The domain helps categorize knowledge.
func (cv *ChromemVectorDB) StoreDocumentWithDomain(concept, content, domain string) ([]string, error) {
	cv.mu.Lock()
	defer cv.mu.Unlock()

	fullContent := concept + "\n\n" + content

	metadata := map[string]string{"concept": concept}
	if domain != "" {
		metadata["domain"] = domain
	}

	// Small texts: store as a single document
	if len(fullContent) <= 4000 {
		ctx := context.Background()
		docID := fmt.Sprintf("mem_%d", time.Now().UnixNano())
		doc := chromem.Document{
			ID:       docID,
			Metadata: metadata,
			Content:  fullContent,
		}
		if err := cv.collection.AddDocument(ctx, doc); err != nil {
			cv.logger.Error("Failed to store document in vector DB", "error", err)
			return nil, fmt.Errorf("failed to add document: %w", err)
		}
		cv.logger.Info("Stored document in long-term memory", "id", docID, "concept", concept, "domain", domain)
		return []string{docID}, nil
	}

	// Large texts: split into chunks and store each individually
	chunks := chunkText(content, 3500, 200)
	baseTime := time.Now().UnixNano()

	var storedIDs []string
	for i, chunk := range chunks {
		ctx := context.Background()
		docID := fmt.Sprintf("mem_%d_chunk_%d", baseTime, i)
		chunkMeta := map[string]string{
			"concept":     concept,
			"chunk_index": fmt.Sprintf("%d/%d", i+1, len(chunks)),
		}
		if domain != "" {
			chunkMeta["domain"] = domain
		}
		doc := chromem.Document{
			ID:       docID,
			Metadata: chunkMeta,
			Content:  concept + "\n\n" + chunk,
		}
		if err := cv.collection.AddDocument(ctx, doc); err != nil {
			cv.logger.Error("Failed to store chunk", "error", err, "chunk", i+1, "total", len(chunks))
			return storedIDs, fmt.Errorf("failed to add chunk %d/%d: %w", i+1, len(chunks), err)
		}
		cv.logger.Debug("Stored chunk", "id", docID, "chunk", i+1, "total", len(chunks), "chars", len(chunk))
		storedIDs = append(storedIDs, docID)
	}

	cv.logger.Info("Stored chunked document in long-term memory", "concept", concept, "domain", domain, "chunks", len(chunks), "total_chars", len(content))
	return storedIDs, nil
}

// chunkText splits a large text into smaller segments of roughly chunkSize characters,
// preferring paragraph (\n\n) or sentence boundaries. Adds overlap characters between chunks.
func chunkText(text string, chunkSize, overlap int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	start := 0

	for start < len(text) {
		end := start + chunkSize
		if end >= len(text) {
			chunks = append(chunks, strings.TrimSpace(text[start:]))
			break
		}

		// Try to split at paragraph boundary (\n\n)
		splitAt := strings.LastIndex(text[start:end], "\n\n")
		if splitAt > chunkSize/2 {
			end = start + splitAt + 2 // include the double newline
		} else {
			// Fall back to sentence boundary (.  or .\n)
			splitAt = strings.LastIndex(text[start:end], ". ")
			if splitAt > chunkSize/2 {
				end = start + splitAt + 2
			}
			// else: hard cut at chunkSize
		}

		chunks = append(chunks, strings.TrimSpace(text[start:end]))

		// Move forward with overlap
		start = end - overlap
		if start < 0 {
			start = 0
		}
	}

	return chunks
}

// StoreBatch stores multiple concept/content pairs, delegating to
// StoreDocumentWithDomain for each item so that large texts are properly chunked.
func (cv *ChromemVectorDB) StoreBatch(items []ArchiveItem) ([]string, error) {
	var allIDs []string

	for _, item := range items {
		ids, err := cv.StoreDocumentWithDomain(item.Concept, item.Content, item.Domain)
		if err != nil {
			cv.logger.Error("Failed to store batch item", "concept", item.Concept, "error", err)
			return allIDs, fmt.Errorf("failed to store batch item %q: %w", item.Concept, err)
		}
		allIDs = append(allIDs, ids...)
	}

	cv.logger.Info("Stored batch in long-term memory", "count", len(items), "total_docs", len(allIDs))
	return allIDs, nil
}

// SearchSimilar finds the topK most semantically similar documents across all relevant collections.
func (cv *ChromemVectorDB) SearchSimilar(query string, topK int) ([]string, []string, error) {
	ctx := context.Background()

	collections := []string{"aurago_memories", "tool_guides", "documentation"}
	var allMemories []string
	var allDocIDs []string

	for _, colName := range collections {
		col, err := cv.db.GetOrCreateCollection(colName, nil, cv.embeddingFunc)
		if err != nil {
			continue
		}
		if col.Count() == 0 {
			cv.logger.Debug("Collection empty, skipping search", "collection", colName)
			continue
		}
		cv.logger.Info("Searching collection", "collection", colName, "docs", col.Count())

		searchK := topK
		if searchK > col.Count() {
			searchK = col.Count()
		}

		results, err := col.Query(ctx, query, searchK, nil, nil)
		if err != nil {
			cv.logger.Warn("Failed to query collection", "collection", colName, "error", err)
			continue
		}

		for _, result := range results {
			if result.Similarity > 0.3 {
				domainHint := ""
				if d, ok := result.Metadata["domain"]; ok && d != "" {
					domainHint = fmt.Sprintf(" [Domain: %s]", d)
				}
				// Tag documentation/tools results for the LLM
				if colName != "aurago_memories" {
					domainHint = fmt.Sprintf(" [%s]", colName)
				}

				cv.logger.Debug("Retrieved memory", "collection", colName, "id", result.ID, "similarity", result.Similarity)
				allMemories = append(allMemories, fmt.Sprintf("[Similarity: %.2f]%s %s", result.Similarity, domainHint, result.Content))
				allDocIDs = append(allDocIDs, result.ID)
			}
		}
	}

	// Dynamic sorting if we have many results from different collections
	// (Optional: for now just return them as they come)

	return allMemories, allDocIDs, nil
}

// GetByID retrieves a document's full content by its ID.
func (cv *ChromemVectorDB) GetByID(id string) (string, error) {
	ctx := context.Background()
	doc, err := cv.collection.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	return doc.Content, nil
}

// DeleteDocument removes a specific document from the VectorDB by its ID.
func (cv *ChromemVectorDB) DeleteDocument(id string) error {
	ctx := context.Background()
	return cv.collection.Delete(ctx, nil, nil, id)
}
