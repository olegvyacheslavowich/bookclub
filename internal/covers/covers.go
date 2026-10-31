// Package covers keeps photos of book covers outside the club's JSON document: on Yandex Disk
// (Disk) or in a local directory (Dir, for development and a VPS).
//
// A cover file is written once and never changed: replacing a cover writes a file with a new name.
// That makes caching trivial — a name always means the same bytes (see Cached).
package covers

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// ErrNotFound: no file with that name.
var ErrNotFound = errors.New("covers: not found")

// ErrCannotPublish: the storage has no public links (a local folder).
var ErrCannotPublish = errors.New("covers: this storage can't publish files")

// Publisher is a storage that can give a file a public link: Disk.
type Publisher interface {
	Publish(ctx context.Context, name string) (string, error)
}

// MaxSize is the largest photo accepted. The front end shrinks photos to a few hundred KB before
// uploading, so this only stops abuse; it also stays below the 3.5 MB request limit of Yandex
// Serverless Containers.
const MaxSize = 3 << 20

// validName: "<book id>-<random>.<ext>", as made by the API. Checked by every backend, so a name
// can never point outside the covers folder.
var validName = regexp.MustCompile(`^[0-9a-f]{1,32}-[0-9a-f]{16}\.(jpg|png|webp)$`)

func checkName(name string) error {
	if !validName.MatchString(name) {
		return errors.New("covers: bad file name " + name)
	}
	return nil
}

// Ext returns the file extension for a supported photo format, judged by the bytes themselves,
// not by what the client claims. ok is false for anything that isn't JPEG, PNG or WebP.
func Ext(data []byte) (ext string, ok bool) {
	switch http.DetectContentType(data) {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	}
	return "", false
}

// ContentType of a stored cover, by its extension.
func ContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
