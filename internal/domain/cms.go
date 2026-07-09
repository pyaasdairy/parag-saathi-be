package domain

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CMS content types (blueprint §6.1). The set is deliberately open for
// extension: a future type (e.g. a "maps" content item) is a new constant +
// AllCMSTypes entry — the module's storage, delta pull and RBAC are all
// type-agnostic and need no other change.
const (
	CMSTypeScheme   = "scheme"
	CMSTypeVideo    = "video"
	CMSTypeArticle  = "article"
	CMSTypeHelpline = "helpline"
)

// AllCMSTypes is the closed set of authored content types (extend here).
var AllCMSTypes = []string{
	CMSTypeScheme, CMSTypeVideo, CMSTypeArticle, CMSTypeHelpline,
}

// CMS region scopes: the org tier a content item is targeted at. "all" is the
// unconditional default (shown everywhere); the others narrow to the org unit
// named by RegionRef.
const (
	CMSScopeState    = "state"
	CMSScopeDistrict = "district"
	CMSScopeSociety  = "society"
	CMSScopeAll      = "all"
)

// AllCMSScopes is the closed set of region scopes (extend here).
var AllCMSScopes = []string{
	CMSScopeState, CMSScopeDistrict, CMSScopeSociety, CMSScopeAll,
}

var cmsTypeSet = sliceSet(AllCMSTypes)
var cmsScopeSet = sliceSet(AllCMSScopes)

func sliceSet(vals []string) map[string]struct{} {
	s := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		s[v] = struct{}{}
	}
	return s
}

// IsValidCMSType reports whether t is one of the content types.
func IsValidCMSType(t string) bool {
	_, ok := cmsTypeSet[t]
	return ok
}

// IsValidCMSScope reports whether s is one of the region scopes.
func IsValidCMSScope(s string) bool {
	_, ok := cmsScopeSet[s]
	return ok
}

// CMSContent is one authored content item served to the field app — a scheme
// card, an explainer video, an article or a Get-Help helpline entry
// (blueprint §6.1). Titles and descriptions are keyed by language code so a
// single item serves every locale. Version is a monotonic cursor: the app
// pulls only items with a version greater than the one it last saw
// (versioned delta sync), and every author write bumps it.
type CMSContent struct {
	ID              primitive.ObjectID  `bson:"_id,omitempty"                json:"id"`
	Type            string              `bson:"type"                         json:"type"`
	TitleI18n       map[string]string   `bson:"title_i18n"                   json:"title_i18n"`
	DescriptionI18n map[string]string   `bson:"description_i18n,omitempty"   json:"description_i18n,omitempty"`
	URL             string              `bson:"url,omitempty"                json:"url,omitempty"`
	ThumbnailURL    string              `bson:"thumbnail_url,omitempty"      json:"thumbnail_url,omitempty"`
	PhoneNumbers    []string            `bson:"phone_numbers,omitempty"      json:"phone_numbers,omitempty"`
	Languages       []string            `bson:"languages,omitempty"          json:"languages,omitempty"`
	RegionScope     string              `bson:"region_scope"                 json:"region_scope"`
	RegionRef       *primitive.ObjectID `bson:"region_ref,omitempty"         json:"region_ref,omitempty"`
	Category        string              `bson:"category,omitempty"           json:"category,omitempty"`
	Order           int                 `bson:"order"                        json:"order"`
	Published       bool                `bson:"published"                    json:"published"`
	Version         int64               `bson:"version"                      json:"version"`
	ValidFrom       *time.Time          `bson:"valid_from,omitempty"         json:"valid_from,omitempty"`
	ValidTo         *time.Time          `bson:"valid_to,omitempty"           json:"valid_to,omitempty"`
	CreatedAt       time.Time           `bson:"created_at"                   json:"created_at"`
	UpdatedAt       time.Time           `bson:"updated_at"                   json:"updated_at"`
}
