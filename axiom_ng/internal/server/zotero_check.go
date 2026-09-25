package server

import (
	"errors"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

// ZoteroChecker adapts a zoteroprovider.Source to the server Checker interface.
type ZoteroChecker struct {
	src zoteroprovider.Source
}

// CheckZotero wraps a zotero source as a health checker.
func CheckZotero(src zoteroprovider.Source) *ZoteroChecker {
	return &ZoteroChecker{src: src}
}

// Ready reports the Zotero local API as ready when a Server-ID is obtainable.
func (c *ZoteroChecker) Ready() error {
	if c == nil || c.src == nil {
		return errors.New("no zotero source configured")
	}
	if c.src.ServerID() == "" {
		return errors.New("zotero local api unreachable (is Zotero running and the local API enabled?)")
	}
	return nil
}
