// Package category classifies downloads into IDM-style categories based on the
// file extension, and resolves the destination folder for each category.
package category

import (
	"path/filepath"
	"strings"
)

// Category identifiers match IDM's default set plus a general bucket.
const (
	General    = "General"
	Compressed = "Compressed"
	Documents  = "Documents"
	Music      = "Music"
	Video      = "Video"
	Programs   = "Programs"
)

// All returns the categories in display order.
func All() []string {
	return []string{General, Compressed, Documents, Music, Video, Programs}
}

var extToCategory = func() map[string]string {
	groups := map[string][]string{
		Compressed: {"zip", "rar", "7z", "tar", "gz", "bz2", "xz", "cab", "iso", "tgz", "zst"},
		Documents:  {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "rtf", "odt", "epub", "csv", "md"},
		Music:      {"mp3", "wav", "flac", "aac", "ogg", "wma", "m4a", "opus"},
		Video:      {"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "ts"},
		Programs:   {"exe", "msi", "msix", "bat", "cmd", "apk", "dmg", "deb", "rpm", "appimage"},
	}
	m := map[string]string{}
	for cat, exts := range groups {
		for _, e := range exts {
			m[e] = cat
		}
	}
	return m
}()

// FromFilename returns the category for a filename based on its extension.
func FromFilename(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if cat, ok := extToCategory[ext]; ok {
		return cat
	}
	return General
}

// FromMIME maps a MIME type to a category as a fallback when the extension is
// unknown. It only handles broad type prefixes.
func FromMIME(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(mime, "video/"):
		return Video
	case strings.HasPrefix(mime, "audio/"):
		return Music
	case strings.HasPrefix(mime, "application/pdf"),
		strings.HasPrefix(mime, "text/"):
		return Documents
	case strings.HasPrefix(mime, "application/zip"),
		strings.HasPrefix(mime, "application/x-7z"),
		strings.HasPrefix(mime, "application/x-rar"),
		strings.HasPrefix(mime, "application/gzip"):
		return Compressed
	default:
		return General
	}
}

// Resolve picks a category from filename first, then MIME.
func Resolve(filename, mime string) string {
	if c := FromFilename(filename); c != General {
		return c
	}
	if c := FromMIME(mime); c != General {
		return c
	}
	return General
}

// DefaultSubfolder returns the conventional subfolder name for a category,
// relative to the user's base download directory.
func DefaultSubfolder(cat string) string {
	switch cat {
	case Compressed:
		return "Compressed"
	case Documents:
		return "Documents"
	case Music:
		return "Music"
	case Video:
		return "Video"
	case Programs:
		return "Programs"
	default:
		return "General"
	}
}
