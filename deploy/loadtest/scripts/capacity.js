// Step-load test of the four critical user journeys (CUJs). `make loadtest-run` runs it as the Job
// in deploy/loadtest/run; docs/capacity/README.md explains the method and how to read the result.
//
// Open model: each CUJ is a ramping-arrival-rate scenario, so k6 starts journeys at the planned rate
// whether or not the shop keeps up. A slow shop shows up as latency and errors, not as less load.
//
// Settings (environment variables, all optional):
//   STEPS                  requests per second for each step, e.g. "10,20,40"
//   STEP_DURATION          length of each step, e.g. "5m" (k6 duration: h, m, s)
//   RAMP                   time to move from one step's rate to the next, inside the step ("10s")
//   MIX                    share of journeys per CUJ, e.g. "browse=40,add-to-cart=10,view-cart=25,checkout=25"
//   LATENCY_THRESHOLDS_MS  latency thresholds counted per request; must include each SLO's threshold
//   TARGET_URL, TARGET_HOST, RUN_ID, MAX_VUS, REQUEST_TIMEOUT
import http from 'k6/http';
import { Counter } from 'k6/metrics';

function env(name, fallback) {
  const value = __ENV[name];
  return value === undefined || value.trim() === '' ? fallback : value.trim();
}

// "1h2m3s", "5m", "90s", "500ms" -> seconds.
function parseDuration(text) {
  const match = /^(?:(\d+)h)?(?:(\d+)m(?!s))?(?:(\d+)s)?(?:(\d+)ms)?$/.exec(text);
  if (!match || text === '') {
    throw new Error(`not a duration: "${text}" (use for example 5m, 90s or 1m30s)`);
  }
  const [, h, m, s, ms] = match.map((part) => Number(part || 0));
  return h * 3600 + m * 60 + s + ms / 1000;
}

function parseNumbers(name, text) {
  const values = text.split(',').map((part) => Number(part.trim()));
  if (values.length === 0 || values.some((v) => !Number.isFinite(v) || v <= 0)) {
    throw new Error(`${name}="${text}": want a comma-separated list of positive numbers`);
  }
  return values;
}

// The CUJ ids, their frontend requests and paths are fixed by the project's build contract
// (docs/prd.md). `requests` is how many HTTP requests one journey sends.
const JOURNEYS = {
  browse: { exec: 'browse', requests: 2 }, // GET /, GET /product/{id}
  'add-to-cart': { exec: 'addToCart', requests: 1 }, // POST /cart
  'view-cart': { exec: 'viewCart', requests: 2 }, // POST /cart (counted as add-to-cart), GET /cart
  checkout: { exec: 'checkout', requests: 2 }, // POST /cart (counted as add-to-cart), POST /cart/checkout
};

function parseMix(text) {
  const mix = {};
  for (const part of text.split(',')) {
    const [id, weight] = part.split('=').map((s) => (s || '').trim());
    if (!(id in JOURNEYS) || !(Number(weight) >= 0)) {
      throw new Error(`MIX="${text}": want id=weight pairs with ids from ${Object.keys(JOURNEYS).join(', ')}`);
    }
    mix[id] = Number(weight);
  }
  const total = Object.values(mix).reduce((a, b) => a + b, 0);
  if (total <= 0) {
    throw new Error(`MIX="${text}": the weights add up to 0`);
  }
  for (const id of Object.keys(mix)) {
    mix[id] /= total;
  }
  return mix;
}

// In the cluster, to the Istio ingress gateway's internal port 8080, with the public Host header its
// Gateway and VirtualService accept. Port 8080 is outside the gateway's public rate-limit bucket; the
// public URL would also go through Cloud Armor's per-IP rate limit.
const BASE_URL = env('TARGET_URL', 'http://istio-ingressgateway.istio-ingress.svc.cluster.local:8080').replace(/\/+$/, '');
const HOST = env('TARGET_HOST', 'mamori.codewithmuh.com');
const RUN_ID = env('RUN_ID', 'manual');
const STEPS = parseNumbers('STEPS', env('STEPS', '10,20'));
const STEP_SECONDS = parseDuration(env('STEP_DURATION', '5m'));
const RAMP_SECONDS = parseDuration(env('RAMP', '10s'));
// Shares of journeys, not requests, weighted towards the cart journeys so that each CUJ gets enough
// requests per step to confirm its SLO.
const MIX = parseMix(env('MIX', 'browse=40,add-to-cart=10,view-cart=25,checkout=25'));
const LATENCY_THRESHOLDS_MS = parseNumbers('LATENCY_THRESHOLDS_MS', env('LATENCY_THRESHOLDS_MS', '100,250,500,750,1000,1500,2000,3000'));
const MAX_VUS = Math.max(1, Math.floor(parseNumbers('MAX_VUS', env('MAX_VUS', '100'))[0]));
const REQUEST_TIMEOUT = env('REQUEST_TIMEOUT', '10s');

if (RAMP_SECONDS >= STEP_SECONDS) {
  throw new Error(`RAMP (${RAMP_SECONDS}s) must be shorter than STEP_DURATION (${STEP_SECONDS}s)`);
}

// Journeys per minute for one CUJ at a step of `rps` requests per second. A per-minute rate keeps the
// rounding error small at low rates.
function journeysPerMinute(rps, id) {
  const requestsPerJourney = Object.keys(MIX).reduce((sum, j) => sum + MIX[j] * JOURNEYS[j].requests, 0);
  return Math.round((rps / requestsPerJourney) * MIX[id] * 60);
}

function scenarios() {
  const out = {};
  for (const id of Object.keys(MIX)) {
    if (MIX[id] === 0) {
      continue;
    }
    const stages = [];
    for (const rps of STEPS) {
      const rate = journeysPerMinute(rps, id);
      stages.push({ duration: `${RAMP_SECONDS}s`, target: rate });
      stages.push({ duration: `${STEP_SECONDS - RAMP_SECONDS}s`, target: rate });
    }
    // Enough VUs for the peak rate at up to three seconds per journey. k6 adds more up to MAX_VUS
    // when the shop slows down, but drops journeys while it starts them (dropped_iterations), so
    // starting with too few would look like a generator that cannot keep up.
    const peakPerSecond = Math.max(...stages.map((s) => s.target)) / 60;
    out[id] = {
      executor: 'ramping-arrival-rate',
      exec: JOURNEYS[id].exec,
      startRate: 0,
      timeUnit: '1m',
      preAllocatedVUs: Math.min(MAX_VUS, Math.max(5, Math.ceil(peakPerSecond * 3))),
      maxVUs: MAX_VUS,
      stages,
      gracefulStop: '30s',
    };
  }
  return out;
}

// Always-true thresholds make k6 keep per-CUJ totals in the end-of-test summary.
function summaryThresholds() {
  const out = {};
  for (const id of Object.keys(JOURNEYS)) {
    out[`mamori_cuj_requests{cuj:${id}}`] = ['count>=0'];
    out[`mamori_cuj_good{cuj:${id}}`] = ['count>=0'];
  }
  return out;
}

export const options = {
  scenarios: scenarios(),
  thresholds: summaryThresholds(),
  // Bodies are only read where a check needs them (checkout); this keeps memory per VU small.
  discardResponseBodies: true,
  // No url tag (one series per product id) and no scenario tag (the cuj tag says the same).
  systemTags: ['status', 'method', 'name', 'expected_response', 'error_code'],
  tags: { run: RUN_ID },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

// Exported as k6_mamori_cuj_{requests,good,fast}_total. tools/loadtest reads them per step.
const requests = new Counter('mamori_cuj_requests');
const good = new Counter('mamori_cuj_good');
// One series per threshold, like histogram buckets: a good request faster than threshold_ms. A failed
// request never counts as fast.
const fast = new Counter('mamori_cuj_fast');

// Series that appear in the middle of a measuring window lose their first sample in increase().
// Each VU creates every series at 0 in its first iteration, during the first step's warm-up.
let seriesCreated = false;
function createSeries() {
  if (seriesCreated) {
    return;
  }
  seriesCreated = true;
  for (const cuj of Object.keys(JOURNEYS)) {
    requests.add(0, { cuj });
    good.add(0, { cuj });
    for (const t of LATENCY_THRESHOLDS_MS) {
      fast.add(0, { cuj, threshold_ms: String(t) });
    }
  }
}

function record(cuj, res, expectedStatus, mustContain) {
  const ok =
    res.error_code === 0 &&
    res.status === expectedStatus &&
    (mustContain === undefined || (typeof res.body === 'string' && res.body.includes(mustContain)));
  requests.add(1, { cuj });
  if (!ok) {
    return;
  }
  good.add(1, { cuj });
  for (const t of LATENCY_THRESHOLDS_MS) {
    if (res.timings.duration < t) {
      fast.add(1, { cuj, threshold_ms: String(t) });
    }
  }
}

function params(cuj, name, extra) {
  // No redirects: POST /cart answers 302, and following it would add a GET /cart to every add-to-cart.
  return Object.assign({ headers: { Host: HOST }, redirects: 0, timeout: REQUEST_TIMEOUT, tags: { cuj, name } }, extra);
}

// Online Boutique v0.10.7's product catalog.
const PRODUCTS = ['0PUK6V6EV0', '1YMWWN1N4O', '2ZYFJ3GM2N', '66VCHSJNUP', '6E92ZMYYFZ', '9SIQT8TOJO', 'L9ECAV7KIM', 'LS4PSXUNUM', 'OLJCESPC7Z'];

function pick(list) {
  return list[Math.floor(Math.random() * list.length)];
}

// Each iteration starts with an empty cookie jar, so every journey is a new session with an empty cart.
function addItem() {
  const body = { product_id: pick(PRODUCTS), quantity: '1' };
  record('add-to-cart', http.post(`${BASE_URL}/cart`, body, params('add-to-cart', 'POST /cart')), 302);
}

export function browse() {
  createSeries();
  record('browse', http.get(`${BASE_URL}/`, params('browse', 'GET /')), 200);
  record('browse', http.get(`${BASE_URL}/product/${pick(PRODUCTS)}`, params('browse', 'GET /product/{id}')), 200);
}

export function addToCart() {
  createSeries();
  addItem();
}

// The dependent request is sent even when adding the item failed, so every journey sends the same
// number of requests and the delivered rate stays comparable with the plan.
export function viewCart() {
  createSeries();
  addItem();
  record('view-cart', http.get(`${BASE_URL}/cart`, params('view-cart', 'GET /cart')), 200);
}

export function checkout() {
  createSeries();
  addItem();
  // The demo's own pre-filled form (src/frontend/templates/cart.html): a fake card, never a real one.
  const order = {
    email: 'someone@example.com',
    street_address: '1600 Amphitheatre Parkway',
    zip_code: '94043',
    city: 'Mountain View',
    state: 'CA',
    country: 'United States',
    credit_card_number: '4432801561520454',
    credit_card_expiration_month: '1',
    credit_card_expiration_year: String(new Date().getUTCFullYear() + 1),
    credit_card_cvv: '672',
  };
  const res = http.post(`${BASE_URL}/cart/checkout`, order, params('checkout', 'POST /cart/checkout', { responseType: 'text' }));
  record('checkout', res, 200, 'Your order is complete!');
}

// The scenarios start right after setup returns, so this is step 1's start. It ends up in the
// summary as setup_data, where `loadtest report --summary` reads the schedule.
export function setup() {
  const plan = {
    runId: RUN_ID,
    start: new Date().toISOString(),
    steps: STEPS,
    stepSeconds: STEP_SECONDS,
    rampSeconds: RAMP_SECONDS,
    mix: MIX,
    latencyThresholdsMs: LATENCY_THRESHOLDS_MS,
    target: BASE_URL,
    host: HOST,
  };
  console.log(`loadtest_start ${JSON.stringify(plan)}`);
  return plan;
}

// The summary goes to stdout between markers, on one line, so `make loadtest-run` can cut it out of
// the Job's log.
export function handleSummary(data) {
  return { stdout: `MAMORI_K6_SUMMARY_BEGIN\n${JSON.stringify(data)}\nMAMORI_K6_SUMMARY_END\n` };
}
