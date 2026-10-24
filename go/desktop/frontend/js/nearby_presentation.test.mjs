import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizePlaceCollection, placeDistanceLabel, safeMapPolicy, placeCategoryLabel } from './nearby_presentation.js';

test('place cards require observed finite coordinates and preserve inert source text', () => {
  const result = normalizePlaceCollection({ query: '<script>query</script>', places: [
    { id: 'one', name: '<img src=x onerror=1>', latitude: 25.04, longitude: 121.5, url: 'https://example.com/a', distanceMeters: 1450 },
    { id: 'one', name: 'Duplicate', latitude: 25.04, longitude: 121.5 },
    { id: 'two', name: 'Impossible', latitude: 125, longitude: 121.5 },
    { id: 'three', name: 'String coordinate', latitude: '25', longitude: 121.5 },
    { id: 'four', name: 'NaN', latitude: NaN, longitude: 121.5 },
    { id: 'five', name: 'Unsafe link', latitude: 25, longitude: 121, url: 'javascript:alert(1)' },
  ] });
  assert.equal(result.places.length, 2);
  assert.equal(result.places[0].name, '<img src=x onerror=1>');
  assert.equal(result.places[0].rating, null);
  assert.equal(result.places[0].ratingCount, null);
  assert.equal(result.places[1].url, null);
  assert.equal(placeDistanceLabel(result.places[0].distanceMeters), '直線距離 1.4 公里');
});

test('missing fields never become ratings, distances or current-location claims', () => {
  const [p] = normalizePlaceCollection({ center: { latitude: 25, longitude: null }, places: [{ name: 'Place', latitude: 0, longitude: 0, rating: '5', distanceMeters: -1 }] }).places;
  assert.equal(p.rating, null);
  assert.equal(p.distanceMeters, null);
  assert.equal(placeDistanceLabel(null), '');
  assert.equal(normalizePlaceCollection({ center: { latitude: 25, longitude: null }, places: [] }).center, null);
  assert.deepEqual(normalizePlaceCollection({ places: [] }).places, []);
});

test('map policy restricts tiles to own session or exact configured public OSM URL', () => {
  const policy = { tileURL: '/session/own/map-tiles/{z}/{x}/{y}', maxZoom: 22, attribution: '<img onerror=1>', browserTileURL: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png' };
  assert.equal(safeMapPolicy(policy, 'other', 'https:'), null);
  assert.equal(safeMapPolicy({ ...policy, tileURL: '/session/own/map-tiles/{z}/{x}/{y}?token=secret' }, 'own'), null);
  assert.equal(safeMapPolicy(policy, 'own', 'https:').tileURL, policy.browserTileURL);
  assert.equal(safeMapPolicy(policy, 'own', 'wails:').tileURL, policy.tileURL);
  assert.equal(safeMapPolicy({ ...policy, browserTileURL: 'https://evil.example/{z}/{x}/{y}' }, 'own', 'https:').tileURL, policy.tileURL);
  assert.equal(safeMapPolicy(policy, 'own', 'https:').maxZoom, 18);
});

test('provider category codes use translated presentation labels or remain omitted', () => {
  assert.equal(placeCategoryLabel('amenity:library'), '圖書館');
  assert.equal(placeCategoryLabel('shop:unknown_internal_code'), '');
});

test('Mapbox uses own-session versioned 512px tiles and cannot select a direct provider URL', () => {
  const policy = { provider: 'mapbox', tileRevision: '1122334455667788', tileURL: '/session/own/map-tiles/{z}/{x}/{y}?v=1122334455667788', tileSize: 512, zoomOffset: -1, browserTileURL: 'https://tile.openstreetmap.org/{z}/{x}/{y}.png' };
  for (const protocol of ['https:', 'wails:']) {
    const safe = safeMapPolicy(policy, 'own', protocol);
    assert.equal(safe.tileURL, policy.tileURL);
    assert.equal(safe.tileSize, 512);
    assert.equal(safe.zoomOffset, -1);
    assert.equal(safe.minZoom, 1);
    assert.equal(safe.provider, 'mapbox');
  }
  assert.equal(safeMapPolicy(policy, 'other', 'https:'), null);
  for (const invalid of [{ tileRevision: '../bad' }, { tileRevision: '' }, { tileSize: 256 }, { zoomOffset: 0 }, { tileURL: `${policy.tileURL}&token=private` }]) {
    assert.equal(safeMapPolicy({ ...policy, ...invalid }, 'own', 'https:'), null);
  }
});
