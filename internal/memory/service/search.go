// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/kognition-ai/kognition/internal/memory"
)

const (
	bm25K1        = 1.2
	bm25B         = 0.75
	reciprocalK   = 60.0
	defaultSearch = 10
)

func rankBM25(records []memory.Record, query string, topK int, minScore float64) []memory.SearchResult {
	queryTerms := tokenize(query)
	if len(records) == 0 || len(queryTerms) == 0 {
		return nil
	}

	documents := make([][]string, len(records))
	documentFrequency := make(map[string]int, len(queryTerms))
	totalLength := 0
	for index, record := range records {
		documents[index] = tokenize(record.Content)
		totalLength += len(documents[index])
		seen := make(map[string]struct{}, len(documents[index]))
		for _, term := range documents[index] {
			seen[term] = struct{}{}
		}
		for term := range seen {
			documentFrequency[term]++
		}
	}

	averageLength := float64(totalLength) / float64(len(documents))
	if averageLength == 0 {
		return nil
	}

	results := make([]memory.SearchResult, 0, len(records))
	for index, terms := range documents {
		termFrequency := make(map[string]int, len(terms))
		for _, term := range terms {
			termFrequency[term]++
		}

		var score float64
		for _, term := range queryTerms {
			frequency := termFrequency[term]
			if frequency == 0 {
				continue
			}
			documentsWithTerm := documentFrequency[term]
			inverseDocumentFrequency := math.Log(1 + (float64(len(documents)-documentsWithTerm)+0.5)/(float64(documentsWithTerm)+0.5))
			lengthNormalization := 1 - bm25B + bm25B*float64(len(terms))/averageLength
			score += inverseDocumentFrequency * (float64(frequency) * (bm25K1 + 1)) /
				(float64(frequency) + bm25K1*lengthNormalization)
		}
		if score > 0 {
			results = append(results, memory.SearchResult{Record: records[index], Score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if !results[i].Record.Timestamp.Equal(results[j].Record.Timestamp) {
			return results[i].Record.Timestamp.After(results[j].Record.Timestamp)
		}
		return results[i].Record.ID < results[j].Record.ID
	})

	if len(results) > 0 {
		maxScore := results[0].Score
		for index := range results {
			results[index].Score /= maxScore
		}
	}
	results = filterMinScore(results, minScore)
	return limitResults(results, topK)
}

func fuseSearchResults(semantic, lexical []memory.SearchResult, topK int) []memory.SearchResult {
	if len(semantic) == 0 {
		return limitResults(lexical, topK)
	}
	if len(lexical) == 0 {
		return limitResults(semantic, topK)
	}

	type fusedResult struct {
		result memory.SearchResult
		score  float64
	}
	fused := make(map[string]*fusedResult, len(semantic)+len(lexical))
	addRanking := func(results []memory.SearchResult) {
		for index, result := range results {
			entry, ok := fused[result.Record.ID]
			if !ok {
				entry = &fusedResult{result: result}
				fused[result.Record.ID] = entry
			}
			entry.score += 1 / (reciprocalK + float64(index+1))
		}
	}
	addRanking(semantic)
	addRanking(lexical)

	results := make([]memory.SearchResult, 0, len(fused))
	for _, entry := range fused {
		entry.result.Score = entry.score
		results = append(results, entry.result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Record.ID < results[j].Record.ID
	})
	if len(results) > 0 {
		maxScore := results[0].Score
		for index := range results {
			results[index].Score /= maxScore
		}
	}
	return limitResults(results, topK)
}

func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func filterMinScore(results []memory.SearchResult, minScore float64) []memory.SearchResult {
	if minScore <= 0 {
		return results
	}
	filtered := results[:0]
	for _, result := range results {
		if result.Score >= minScore {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func limitResults(results []memory.SearchResult, topK int) []memory.SearchResult {
	if topK <= 0 {
		topK = defaultSearch
	}
	if len(results) > topK {
		return results[:topK]
	}
	return results
}
