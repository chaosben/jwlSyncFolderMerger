// Package model holds an ID-free representation of a JW Library user
// database. Every row is identified by a natural key (GUIDs, location tuples,
// content hashes) so that data from different devices can be compared.
package model

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MarkerKeySymbol identifies the hidden Location row that carries the
// generation id of a merged current.jwlibrary. JW Library never deletes
// Location rows, so the marker survives a restore and the next backup.
const MarkerKeySymbol = "jwlSyncFolderMerger"

// Location is a row of the Location table without its id.
type Location struct {
	BookNumber, ChapterNumber, DocumentID, Track, MepsLanguage sql.NullInt64
	IssueTagNumber                                             int64
	KeySymbol                                                  sql.NullString
	Type                                                       int64
	Title, Specialty, Edition                                  sql.NullString
}

// LocKey is the natural key of a Location (everything except Title;
// Specialty/Edition are coalesced like in IX_Location_Media).
type LocKey struct {
	BookNumber, ChapterNumber, DocumentID, Track, MepsLanguage sql.NullInt64
	IssueTagNumber, Type                                       int64
	KeySymbol                                                  sql.NullString
	Specialty, Edition                                         string
}

func (l Location) Key() LocKey {
	return LocKey{
		BookNumber: l.BookNumber, ChapterNumber: l.ChapterNumber, DocumentID: l.DocumentID,
		Track: l.Track, MepsLanguage: l.MepsLanguage, IssueTagNumber: l.IssueTagNumber,
		Type: l.Type, KeySymbol: l.KeySymbol, Specialty: l.Specialty.String, Edition: l.Edition.String,
	}
}

func (k LocKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%d|%s|%s",
		ns(k.KeySymbol), ni(k.MepsLanguage), ni(k.BookNumber), ni(k.ChapterNumber), ni(k.DocumentID),
		ni(k.Track), k.IssueTagNumber, k.Type, k.Specialty, k.Edition)
}

func ni(v sql.NullInt64) string {
	if !v.Valid {
		return "-"
	}
	return fmt.Sprint(v.Int64)
}

func ns(v sql.NullString) string {
	if !v.Valid {
		return "-"
	}
	return v.String
}

type BlockRange struct {
	BlockType, Identifier int64
	StartToken, EndToken  sql.NullInt64
}

type UserMark struct {
	GUID                   string
	Loc                    LocKey
	ColorIndex, StyleIndex int64
	Version                int64
	Ranges                 []BlockRange // sorted
}

type Note struct {
	GUID            string
	UserMarkGUID    sql.NullString
	HasLoc          bool
	Loc             LocKey
	Title, Content  sql.NullString
	LastModified    string
	Created         string
	BlockType       int64
	BlockIdentifier sql.NullInt64
}

type TagKey struct {
	Type int64
	Name string
}

// TargetKind is the kind of object a TagMap row points at.
type TargetKind byte

const (
	TargetNote     TargetKind = 'N'
	TargetLocation TargetKind = 'L'
	TargetPlaylist TargetKind = 'P'
)

// Target identifies what a TagMap row points at.
type Target struct {
	Kind     TargetKind
	Note     string // note GUID
	Loc      LocKey
	Playlist string // playlist item signature
}

func (t Target) String() string {
	switch t.Kind {
	case TargetNote:
		return "N:" + t.Note
	case TargetLocation:
		return "L:" + t.Loc.String()
	default:
		return "P:" + t.Playlist
	}
}

type TagMap struct {
	Tag      TagKey
	Target   Target
	Position int64
}

type BookmarkSlot struct {
	PubLoc LocKey
	Slot   int64
}

type Bookmark struct {
	Loc             LocKey
	Title           string
	Snippet         sql.NullString
	BlockType       int64
	BlockIdentifier sql.NullInt64
}

type InputFieldKey struct {
	Loc     LocKey
	TextTag string
}

// Media is a row of IndependentMedia; Hash is the SHA-256 of the file.
type Media struct {
	OriginalFilename, FilePath, MimeType, Hash string
}

type PlaylistLoc struct {
	Loc                 LocKey
	MajorMultimediaType int64
	BaseDurationTicks   sql.NullInt64
}

type PlaylistMedia struct {
	MediaHash     string
	DurationTicks int64
}

type MarkerParagraph struct {
	MepsDocumentID, ParagraphIndex, MarkerIndexWithinParagraph int64
}

type PlaylistMarker struct {
	Label                                                     string
	StartTimeTicks, DurationTicks, EndTransitionDurationTicks int64
	Verses                                                    []int64
	Paragraphs                                                []MarkerParagraph
}

type PlaylistItem struct {
	Label                                    string
	StartTrimOffsetTicks, EndTrimOffsetTicks sql.NullInt64
	Accuracy, EndAction                      int64
	ThumbnailHash                            string // media hash, "" if none
	Locs                                     []PlaylistLoc
	Medias                                   []PlaylistMedia
	Markers                                  []PlaylistMarker
}

// Signature identifies a playlist item by its complete content.
func (p PlaylistItem) Signature() string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// DB is the ID-free content of one user database.
type DB struct {
	SchemaVersion int
	Marker        string // generation id of a restored current, "" if none
	Locations     map[LocKey]Location
	UserMarks     map[string]UserMark // by GUID
	Notes         map[string]Note     // by GUID
	Tags          map[TagKey]bool
	TagMaps       []TagMap
	Bookmarks     map[BookmarkSlot]Bookmark
	InputFields   map[InputFieldKey]string
	Media         map[string]Media        // by hash
	Playlists     map[string]PlaylistItem // by signature
	Warnings      []string                // dangling references dropped while loading
}

func NewDB() *DB {
	return &DB{
		Locations:   map[LocKey]Location{},
		UserMarks:   map[string]UserMark{},
		Notes:       map[string]Note{},
		Tags:        map[TagKey]bool{},
		Bookmarks:   map[BookmarkSlot]Bookmark{},
		InputFields: map[InputFieldKey]string{},
		Media:       map[string]Media{},
		Playlists:   map[string]PlaylistItem{},
	}
}

// SortRanges orders block ranges canonically.
func SortRanges(r []BlockRange) {
	sort.Slice(r, func(i, j int) bool {
		a, b := r[i], r[j]
		if a.BlockType != b.BlockType {
			return a.BlockType < b.BlockType
		}
		if a.Identifier != b.Identifier {
			return a.Identifier < b.Identifier
		}
		if a.StartToken.Int64 != b.StartToken.Int64 {
			return a.StartToken.Int64 < b.StartToken.Int64
		}
		return a.EndToken.Int64 < b.EndToken.Int64
	})
}

// Canon renders values as a stable string for comparisons.
func Canon(parts ...any) string {
	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteByte('\x1f')
		}
		switch v := p.(type) {
		case sql.NullString:
			sb.WriteString(ns(v))
		case sql.NullInt64:
			sb.WriteString(ni(v))
		case LocKey:
			sb.WriteString(v.String())
		default:
			fmt.Fprint(&sb, v)
		}
	}
	return sb.String()
}
