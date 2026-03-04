package memory

import (
	"strings"
)

// ChunkText splits text into chunks at paragraph boundaries.
// Each chunk includes its starting line number in the source file.
func ChunkText(text string, maxChunkLen int) []TextChunk {
	if maxChunkLen <= 0 {
		maxChunkLen = 1000
	}

	lines := strings.Split(text, "\n")
	var chunks []TextChunk
	var current strings.Builder
	startLine := 1

	flush := func(endLine int) {
		content := strings.TrimSpace(current.String())
		if content != "" {
			chunks = append(chunks, TextChunk{
				Text:      content,
				StartLine: startLine,
				EndLine:   endLine,
			})
		}
		current.Reset()
		startLine = endLine + 1
	}

	for i, line := range lines {
		lineNum := i + 1

		// Paragraph boundary: empty line
		if strings.TrimSpace(line) == "" && current.Len() > 0 {
			// If current chunk is large enough, flush
			if current.Len() >= maxChunkLen/2 {
				flush(lineNum - 1)
				continue
			}
		}

		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)

		// Force flush if too large
		if current.Len() >= maxChunkLen {
			flush(lineNum)
		}
	}

	// Flush remaining
	if current.Len() > 0 {
		flush(len(lines))
	}

	return chunks
}

// TextChunk is a chunk of text with line number metadata.
type TextChunk struct {
	Text      string
	StartLine int
	EndLine   int
}
