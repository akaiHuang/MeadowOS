package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"
)

type LocationResult struct {
	Status         string   `json:"status"`
	Latitude       *float64 `json:"latitude,omitempty"`
	Longitude      *float64 `json:"longitude,omitempty"`
	AccuracyMeters float64  `json:"accuracyMeters,omitempty"`
	Area           string   `json:"area,omitempty"`
	ObservedAt     string   `json:"observedAt,omitempty"`
	LocationRef    string   `json:"locationRef,omitempty"`
}

type LocationRequester func(context.Context, string) (LocationResult, error)

// LocationResolver resolves an opaque, owning-session grant. Only the runtime
// installs this callback; a tool argument can never manufacture coordinates.
type LocationResolver func(context.Context, string) (LocationResult, error)

type Place struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Latitude       float64  `json:"latitude"`
	Longitude      float64  `json:"longitude"`
	Address        string   `json:"address,omitempty"`
	Category       string   `json:"category,omitempty"`
	URL            string   `json:"url,omitempty"`
	DistanceMeters *float64 `json:"distanceMeters,omitempty"`
	Rating         *float64 `json:"rating,omitempty"`
	RatingCount    *int     `json:"ratingCount,omitempty"`
	PhotoURL       string   `json:"photoUrl,omitempty"`
}

type PlaceCenter struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Label     string  `json:"label,omitempty"`
	Source    string  `json:"source"`
}

// UnmarshalJSON preserves the distinction between the real coordinate zero and
// missing/null provider or persisted data. Plain float64 decoding loses it.
func (place *Place) UnmarshalJSON(data []byte) error {
	type placeFields Place
	var fields placeFields
	wire := struct {
		*placeFields
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}{placeFields: &fields}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Latitude == nil || wire.Longitude == nil || !validCoordinates(*wire.Latitude, *wire.Longitude) {
		return fmt.Errorf("place requires explicit valid latitude and longitude")
	}
	fields.Latitude, fields.Longitude = *wire.Latitude, *wire.Longitude
	*place = Place(fields)
	return nil
}

func (center *PlaceCenter) UnmarshalJSON(data []byte) error {
	type centerFields PlaceCenter
	var fields centerFields
	wire := struct {
		*centerFields
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}{centerFields: &fields}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Latitude == nil || wire.Longitude == nil || !validCoordinates(*wire.Latitude, *wire.Longitude) {
		return fmt.Errorf("place center requires explicit valid latitude and longitude")
	}
	fields.Latitude, fields.Longitude = *wire.Latitude, *wire.Longitude
	*center = PlaceCenter(fields)
	return nil
}

type PlaceCollection struct {
	Query       string       `json:"query"`
	Center      *PlaceCenter `json:"center,omitempty"`
	Places      []Place      `json:"places"`
	Provider    string       `json:"provider"`
	Attribution string       `json:"attribution"`
	ObservedAt  string       `json:"observedAt"`
}

func validCoordinates(latitude, longitude float64) bool {
	return !math.IsNaN(latitude) && !math.IsInf(latitude, 0) && latitude >= -90 && latitude <= 90 &&
		!math.IsNaN(longitude) && !math.IsInf(longitude, 0) && longitude >= -180 && longitude <= 180
}

func approximateCoordinate(value float64) float64 { return math.Round(value*1000) / 1000 }

// NormalizeLocationResult strips location from declined responses and reduces
// device coordinates to roughly a city block before they can enter history or
// an external lookup. It never substitutes the server's location.
func NormalizeLocationResult(result LocationResult) (LocationResult, error) {
	// A client payload cannot assign itself a runtime capability. The runtime
	// issues a fresh reference only after normalizing an accepted response.
	result.LocationRef = ""
	result.Area = strings.TrimSpace(result.Area)
	if len(result.Area) > 512 {
		return LocationResult{}, fmt.Errorf("location area exceeds 512 bytes")
	}
	if result.ObservedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, result.ObservedAt); err != nil {
			return LocationResult{}, fmt.Errorf("invalid location timestamp")
		}
	}
	switch result.Status {
	case "granted":
		if result.Latitude == nil || result.Longitude == nil || !validCoordinates(*result.Latitude, *result.Longitude) {
			return LocationResult{}, fmt.Errorf("granted location requires valid coordinates")
		}
		if math.IsNaN(result.AccuracyMeters) || math.IsInf(result.AccuracyMeters, 0) || result.AccuracyMeters < 0 || result.AccuracyMeters > 100000 {
			return LocationResult{}, fmt.Errorf("invalid location accuracy")
		}
		lat, lon := approximateCoordinate(*result.Latitude), approximateCoordinate(*result.Longitude)
		result.Latitude, result.Longitude = &lat, &lon
		// Do not claim precision greater than the rounded coordinates provide.
		result.AccuracyMeters = max(110, result.AccuracyMeters)
	case "manual":
		if result.Area == "" {
			return LocationResult{}, fmt.Errorf("manual location requires an area")
		}
		result.Latitude, result.Longitude, result.AccuracyMeters = nil, nil, 0
	case "denied", "unavailable", "cancelled":
		result.Latitude, result.Longitude, result.AccuracyMeters, result.Area = nil, nil, 0, ""
	default:
		return LocationResult{}, fmt.Errorf("invalid location status")
	}
	return result, nil
}

func safePlaceURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Hostname() == "" || u.User != nil || publicHTTPHostnameIsBlocked(u.Hostname()) {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip == nil || !publicHTTPIPIsBlocked(ip)
}

// ValidatePlaceCollection is also used when restoring persisted provider
// evidence. No HTML, model-generated coordinate or inferred rating is needed.
func ValidatePlaceCollection(collection *PlaceCollection) error {
	if collection == nil {
		return fmt.Errorf("missing place collection")
	}
	if strings.TrimSpace(collection.Query) == "" || len(collection.Query) > 512 ||
		len(collection.Places) > placesMaxResults || len(collection.Provider) == 0 || len(collection.Provider) > 128 ||
		len(collection.Attribution) > 512 {
		return fmt.Errorf("invalid place collection bounds")
	}
	if _, err := time.Parse(time.RFC3339Nano, collection.ObservedAt); err != nil {
		return fmt.Errorf("invalid place observation timestamp")
	}
	if center := collection.Center; center != nil {
		if !validCoordinates(center.Latitude, center.Longitude) || len(center.Label) > 512 || len(center.Source) > 64 {
			return fmt.Errorf("invalid place center")
		}
	}
	seen := make(map[string]bool, len(collection.Places))
	for _, place := range collection.Places {
		if place.ID == "" || len(place.ID) > 128 || seen[place.ID] || strings.TrimSpace(place.Name) == "" || len(place.Name) > 512 ||
			len(place.Address) > 1024 || len(place.Category) > 128 || !validCoordinates(place.Latitude, place.Longitude) ||
			!safePlaceURL(place.URL) || !safePlaceURL(place.PhotoURL) {
			return fmt.Errorf("invalid place record")
		}
		seen[place.ID] = true
		if value := place.DistanceMeters; value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > placesMaxRadiusMeters) {
			return fmt.Errorf("invalid place distance")
		}
		if value := place.Rating; value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 5) {
			return fmt.Errorf("invalid place rating")
		}
		if place.RatingCount != nil && (*place.RatingCount < 0 || *place.RatingCount > 1_000_000_000) {
			return fmt.Errorf("invalid place rating count")
		}
	}
	return nil
}

func ClonePlaceCollection(collection *PlaceCollection) *PlaceCollection {
	if collection == nil {
		return nil
	}
	clone := *collection
	if collection.Center != nil {
		center := *collection.Center
		clone.Center = &center
	}
	clone.Places = make([]Place, len(collection.Places))
	for i, place := range collection.Places {
		clone.Places[i] = place
		if place.DistanceMeters != nil {
			value := *place.DistanceMeters
			clone.Places[i].DistanceMeters = &value
		}
		if place.Rating != nil {
			value := *place.Rating
			clone.Places[i].Rating = &value
		}
		if place.RatingCount != nil {
			value := *place.RatingCount
			clone.Places[i].RatingCount = &value
		}
	}
	return &clone
}
