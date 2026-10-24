package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	placesMaxResults      = 10
	placesMaxRadiusMeters = 50000
	placesTimeout         = 20 * time.Second
	placesResponseBytes   = 1024 * 1024
	placesCacheEntries    = 32
	placesCacheTTL        = 5 * time.Minute
)

// The public endpoint is for modest use. A single process-wide limiter bounds
// requests across sessions, while responses are never cached across sessions.
var placesPublicLimiter = &placesRateLimiter{interval: time.Second}

type placesRateLimiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func (limiter *placesRateLimiter) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		limiter.mu.Lock()
		now := time.Now()
		if !now.Before(limiter.next) {
			limiter.next = now.Add(limiter.interval)
			limiter.mu.Unlock()
			return nil
		}
		delay := time.Until(limiter.next)
		limiter.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type placesCacheEntry struct {
	collection *PlaceCollection
	expires    time.Time
}

// SearchPlaces looks up actual Photon/OSM features. HTTP and BaseURL are
// injection/configuration seams, not model arguments. A new instance is bound
// per session so even approximate locations cannot cross session caches.
type SearchPlaces struct {
	BaseURL         string
	HTTP            *http.Client
	ResolveLocation LocationResolver
	mu              sync.Mutex
	cache           map[string]placesCacheEntry
	// Unexported test-only seams; public URL/DNS policy remains mandatory.
	networkPolicy *publicHTTPPolicy
	limiter       *placesRateLimiter
}

func (t *SearchPlaces) Name() string { return "search_places" }
func (t *SearchPlaces) Kind() Kind   { return KindReadOnly }
func (t *SearchPlaces) Description() string {
	return "Find real places and map results near a named area or an approved location. Pass a user-named area or landmark directly as near; this tool geocodes it. After get_current_location, pass its locationRef as location_ref. Raw or estimated coordinates are not accepted. query names the place/address to find, not the center; for a nearby category supply an OSM category such as amenity:cafe or shop:books (query optional). Every returned place includes mapsSearchUrl: include it beside the place name in your answer. This is a Google Maps search/navigation link, not a verified Google business page; url remains the actual provider source. Coverage may be incomplete; empty results are not proof no places exist. Web search can research programme details."
}
func (t *SearchPlaces) Schema() map[string]any {
	return obj(map[string]any{
		"query":         str("Place name/address to locate. Required without category; optional description when category is supplied."),
		"near":          str("User-named area, address or landmark to use as the center; this tool geocodes it. Use either near OR location_ref."),
		"location_ref":  str("Exact locationRef returned by get_current_location in this run. Resolves the approved device location or manual area; do not invent references."),
		"radius_meters": num("Straight-line search radius, 100-50000 metres; default 5000."),
		"limit":         num("Maximum places, 1-10; default 6."),
		"category":      str("Optional exact OSM key:value category, e.g. amenity:cafe, amenity:school, shop:books. The model chooses this only when appropriate."),
	})
}

// NewSessionInstance preserves trusted provider setup, not a previous session's
// result cache. It is safe to invoke on a shared base registry instance.
func (t *SearchPlaces) NewSessionInstance() *SearchPlaces {
	return &SearchPlaces{BaseURL: t.BaseURL, HTTP: t.HTTP, ResolveLocation: t.ResolveLocation, networkPolicy: t.networkPolicy, limiter: t.limiter}
}

var placesCategoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}:[a-z][a-z0-9_]{0,47}$`)

func placesNumber(args map[string]any, key string) (float64, bool) {
	switch value := args[key].(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		number, err := value.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func (t *SearchPlaces) endpoint() (*url.URL, error) {
	raw := strings.TrimSpace(t.BaseURL)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("MEADOW_PLACES_BASE_URL"))
	}
	if raw == "" {
		raw = "https://photon.komoot.io"
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("place provider must be an HTTPS base URL without credentials, query or fragment")
	}
	return endpoint, nil
}

func (t *SearchPlaces) Execute(ctx context.Context, args map[string]any) Result {
	ctx, cancel := context.WithTimeout(ctx, placesTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return errResult(ctx.Err())
	}
	if _, supplied := args["latitude"]; supplied {
		return errResultf("raw coordinates are not accepted; use near for a named area or location_ref returned by get_current_location")
	}
	if _, supplied := args["longitude"]; supplied {
		return errResultf("raw coordinates are not accepted; use near for a named area or location_ref returned by get_current_location")
	}
	query := strings.TrimSpace(strArg(args, "query"))
	near := strings.TrimSpace(strArg(args, "near"))
	locationRef := strings.TrimSpace(strArg(args, "location_ref"))
	if len(query) > 512 || len(near) > 512 {
		return errResultf("query and area must be bounded text (maximum 512 bytes each)")
	}
	category := strings.TrimSpace(strArg(args, "category"))
	if category != "" && !placesCategoryPattern.MatchString(category) {
		return errResultf("category must be an OSM key:value such as amenity:cafe")
	}
	if query == "" {
		if category == "" {
			return errResultf("query is required when category is not supplied")
		}
		// The exact model-selected category drives the provider request. This
		// generic label is only presentation metadata for category-only calls.
		query = "附近地點"
	}
	radius, limit := float64(5000), 6
	if _, present := args["radius_meters"]; present {
		value, ok := placesNumber(args, "radius_meters")
		if !ok || math.IsNaN(value) || value < 100 || value > placesMaxRadiusMeters {
			return errResultf("radius_meters must be between 100 and 50000")
		}
		radius = value
	}
	if _, present := args["limit"]; present {
		value, ok := placesNumber(args, "limit")
		if !ok || math.IsNaN(value) || value < 1 || value > placesMaxResults || math.Trunc(value) != value {
			return errResultf("limit must be an integer between 1 and 10")
		}
		limit = int(value)
	}
	if (near == "") == (locationRef == "") || len(locationRef) > 512 {
		return errResultf("provide either a named near area or an approved location_ref from get_current_location")
	}
	lat, lon := 0.0, 0.0
	centerSource := "photon_area_lookup"
	if locationRef != "" {
		if t.ResolveLocation == nil {
			return errResultf("location_ref is unavailable in this session; use a named near area")
		}
		resolved, resolveErr := t.ResolveLocation(ctx, locationRef)
		if resolveErr != nil {
			return errResultf("location_ref is invalid, expired, or belongs to another run; use the current get_current_location result or a named near area")
		}
		location, normalizeErr := NormalizeLocationResult(resolved)
		if normalizeErr != nil {
			return errResultf("approved location is invalid")
		}
		switch location.Status {
		case "granted":
			lat, lon, centerSource = *location.Latitude, *location.Longitude, "device_location"
		case "manual":
			near, centerSource = location.Area, "manual_area_lookup"
		default:
			return errResultf("location was not shared; supply a named near area")
		}
	}
	endpoint, err := t.endpoint()
	if err != nil {
		return errResult(err)
	}
	// Resolve the grant before consulting cache: a revoked reference must never
	// recover a previously cached result. Source is part of the truthful receipt.
	keyBytes, _ := json.Marshal([]any{endpoint.String(), query, near, lat, lon, centerSource, radius, limit, category})
	keyHash := sha256.Sum256(keyBytes)
	key := hex.EncodeToString(keyHash[:])
	if cached := t.cached(key); cached != nil {
		return placesResult(cached)
	}
	center := &PlaceCenter{Latitude: lat, Longitude: lon, Source: centerSource}
	if near != "" {
		features, lookupErr := t.request(ctx, endpoint, "api", url.Values{"q": {near}, "limit": {"1"}})
		if lookupErr != nil {
			return errResult(lookupErr)
		}
		if len(features) == 0 {
			return errResultf("the place provider could not locate the supplied area; supply a more specific named area")
		}
		place, valid := photonPlace(features[0])
		if !valid {
			return errResultf("the place provider returned no valid area coordinates")
		}
		center = &PlaceCenter{Latitude: place.Latitude, Longitude: place.Longitude, Label: place.Name, Source: centerSource}
	}
	params := url.Values{"lat": {strconv.FormatFloat(center.Latitude, 'f', -1, 64)}, "lon": {strconv.FormatFloat(center.Longitude, 'f', -1, 64)}, "limit": {"50"}}
	path := "api"
	if category != "" {
		path = "reverse"
		params.Set("osm_tag", category)
		params.Set("radius", strconv.FormatFloat(radius/1000, 'f', -1, 64))
	} else {
		params.Set("q", query)
		params.Set("location_bias_scale", "0.1")
		params.Set("bbox", placesBoundingBox(center.Latitude, center.Longitude, radius))
	}
	features, err := t.request(ctx, endpoint, path, params)
	if err != nil {
		return errResult(err)
	}
	collection := &PlaceCollection{Query: query, Center: center, Places: []Place{}, Provider: "Photon", Attribution: "© OpenStreetMap contributors · Photon", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	seen := map[string]bool{}
	validFeatures := 0
	for _, feature := range features {
		place, valid := photonPlace(feature)
		if !valid {
			continue
		}
		validFeatures++
		if seen[place.ID] || (category != "" && place.Category != category) {
			continue
		}
		distance := placesDistance(center.Latitude, center.Longitude, place.Latitude, place.Longitude)
		if distance > radius {
			continue
		}
		// Retain provider coordinates and compute only the geometric distance.
		place.DistanceMeters = &distance
		seen[place.ID] = true
		collection.Places = append(collection.Places, place)
	}
	if len(features) > 0 && validFeatures == 0 {
		return errResultf("place provider returned no valid place records")
	}
	sort.SliceStable(collection.Places, func(i, j int) bool {
		return *collection.Places[i].DistanceMeters < *collection.Places[j].DistanceMeters
	})
	if len(collection.Places) > limit {
		collection.Places = collection.Places[:limit]
	}
	if err := ValidatePlaceCollection(collection); err != nil {
		return errResult(err)
	}
	t.store(key, collection)
	return placesResult(collection)
}

func placesResult(collection *PlaceCollection) Result {
	clone := ClonePlaceCollection(collection)
	if err := ValidatePlaceCollection(clone); err != nil {
		return errResult(err)
	}
	type linkedPlace struct {
		Place
		MapsSearchURL string `json:"mapsSearchUrl"`
	}
	projection := struct {
		*PlaceCollection
		Places []linkedPlace `json:"places"`
	}{PlaceCollection: clone, Places: make([]linkedPlace, 0, len(clone.Places))}
	for _, place := range clone.Places {
		projection.Places = append(projection.Places, linkedPlace{Place: place, MapsSearchURL: googleMapsSearchURL(place)})
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return errResult(err)
	}
	return Result{LLMContent: string(encoded), Display: fmt.Sprintf("%d places via Photon", len(clone.Places)), Places: clone}
}

// Match the native cards' public destination convention. Never serialize a
// device center/location reference or infer a Google place ID from an OSM ID.
func googleMapsSearchURL(place Place) string {
	if !validCoordinates(place.Latitude, place.Longitude) {
		return ""
	}
	bounded := func(text string, limit int) string {
		runes := []rune(strings.TrimSpace(text))
		if len(runes) > limit {
			runes = runes[:limit]
		}
		return string(runes)
	}
	name, address := bounded(place.Name, 300), bounded(place.Address, 500)
	coordinates := strconv.FormatFloat(place.Latitude, 'f', -1, 64) + "," + strconv.FormatFloat(place.Longitude, 'f', -1, 64)
	query := coordinates
	if name != "" && address != "" {
		query = name + ", " + address
	}
	params := url.Values{"api": {"1"}, "query": {query}}
	value := "https://www.google.com/maps/search/?" + params.Encode()
	if len(value) > 2048 {
		params.Set("query", coordinates)
		value = "https://www.google.com/maps/search/?" + params.Encode()
	}
	return value
}

func (t *SearchPlaces) cached(key string) *PlaceCollection {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.cache[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expires) {
		delete(t.cache, key)
		return nil
	}
	return ClonePlaceCollection(entry.collection)
}
func (t *SearchPlaces) store(key string, collection *PlaceCollection) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cache == nil {
		t.cache = make(map[string]placesCacheEntry)
	}
	if len(t.cache) >= placesCacheEntries {
		oldestKey := ""
		var oldest time.Time
		for k, entry := range t.cache {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = k, entry.expires
			}
		}
		delete(t.cache, oldestKey)
	}
	t.cache[key] = placesCacheEntry{collection: ClonePlaceCollection(collection), expires: time.Now().Add(placesCacheTTL)}
}

type photonFeature struct {
	Type     string `json:"type"`
	Geometry struct {
		Type        string     `json:"type"`
		Coordinates []*float64 `json:"coordinates"`
	} `json:"geometry"`
	Properties struct {
		Name        string      `json:"name"`
		Street      string      `json:"street"`
		HouseNumber string      `json:"housenumber"`
		Postcode    string      `json:"postcode"`
		District    string      `json:"district"`
		City        string      `json:"city"`
		State       string      `json:"state"`
		Country     string      `json:"country"`
		OSMKey      string      `json:"osm_key"`
		OSMValue    string      `json:"osm_value"`
		OSMType     string      `json:"osm_type"`
		OSMID       json.Number `json:"osm_id"`
	} `json:"properties"`
}

func (t *SearchPlaces) request(ctx context.Context, endpoint *url.URL, path string, params url.Values) ([]photonFeature, error) {
	limiter := t.limiter
	if limiter == nil {
		limiter = placesPublicLimiter
	}
	if err := limiter.wait(ctx); err != nil {
		return nil, fmt.Errorf("place lookup cancelled or timed out")
	}
	target := *endpoint
	target.Path = strings.TrimRight(endpoint.Path, "/") + "/" + path + "/"
	target.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid place provider request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MeadowDesktop/1.0")
	base := t.HTTP
	if base == nil {
		base = &http.Client{Timeout: placesTimeout}
	}
	client := guardPublicHTTPClient(base, false, t.networkPolicy)
	guardRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		// Do not forward an approximate location to a different provider or to
		// cleartext HTTP, even if the new address would be publicly reachable.
		if request.URL.Scheme != "https" || request.URL.Host != endpoint.Host || request.URL.User != nil {
			return fmt.Errorf("place provider redirect rejected")
		}
		return guardRedirect(request, via)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("place provider is unavailable or blocked by public-network policy")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("place provider returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, placesResponseBytes+1))
	if err != nil || len(body) > placesResponseBytes {
		return nil, fmt.Errorf("place provider response is unreadable or exceeds 1 MiB")
	}
	var decoded struct {
		Features *[]photonFeature `json:"features"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Features == nil || len(*decoded.Features) > 1000 {
		return nil, fmt.Errorf("place provider returned invalid GeoJSON")
	}
	return *decoded.Features, nil
}

func photonPlace(feature photonFeature) (Place, bool) {
	if feature.Type != "Feature" || feature.Geometry.Type != "Point" || len(feature.Geometry.Coordinates) != 2 || feature.Geometry.Coordinates[0] == nil || feature.Geometry.Coordinates[1] == nil {
		return Place{}, false
	}
	lon, lat := *feature.Geometry.Coordinates[0], *feature.Geometry.Coordinates[1]
	properties := feature.Properties
	if !validCoordinates(lat, lon) || strings.TrimSpace(properties.Name) == "" || len(properties.Name) > 512 {
		return Place{}, false
	}
	id, err := strconv.ParseUint(string(properties.OSMID), 10, 64)
	if err != nil || id == 0 {
		return Place{}, false
	}
	var kind string
	switch properties.OSMType {
	case "N":
		kind = "node"
	case "W":
		kind = "way"
	case "R":
		kind = "relation"
	default:
		return Place{}, false
	}
	address := []string{}
	for _, component := range []string{strings.TrimSpace(properties.Street + " " + properties.HouseNumber), properties.District, properties.City, properties.State, properties.Postcode, properties.Country} {
		component = strings.TrimSpace(component)
		if component != "" && (len(address) == 0 || address[len(address)-1] != component) {
			address = append(address, component)
		}
	}
	place := Place{ID: fmt.Sprintf("osm:%s:%d", kind, id), Name: strings.TrimSpace(properties.Name), Latitude: lat, Longitude: lon, Address: strings.Join(address, ", "), URL: fmt.Sprintf("https://www.openstreetmap.org/%s/%d", kind, id)}
	if properties.OSMKey != "" && properties.OSMValue != "" {
		place.Category = properties.OSMKey + ":" + properties.OSMValue
	}
	if len(place.Address) > 1024 || len(place.Category) > 128 {
		return Place{}, false
	}
	return place, true
}

func placesDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const radians = math.Pi / 180
	dLat, dLon := (lat2-lat1)*radians, (lon2-lon1)*radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*radians)*math.Cos(lat2*radians)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 6371000 * 2 * math.Atan2(math.Sqrt(min(1, max(0, a))), math.Sqrt(max(0, 1-a)))
}

func placesBoundingBox(latitude, longitude, radius float64) string {
	latDelta := radius / 6371000 * 180 / math.Pi
	minLat, maxLat := max(-90, latitude-latDelta), min(90, latitude+latDelta)
	minLon, maxLon := float64(-180), float64(180)
	if minLat > -90 && maxLat < 90 {
		lonDelta := math.Asin(min(1, math.Sin(radius/6371000)/math.Cos(latitude*math.Pi/180))) * 180 / math.Pi
		// For a dateline crossing use full longitude bounds and enforce the
		// exact radius on every result; never send an inverted bounding box.
		if longitude-lonDelta >= -180 && longitude+lonDelta <= 180 {
			minLon, maxLon = longitude-lonDelta, longitude+lonDelta
		}
	}
	return fmt.Sprintf("%.8f,%.8f,%.8f,%.8f", minLon, minLat, maxLon, maxLat)
}
