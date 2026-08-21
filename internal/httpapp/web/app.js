const elements = {
  body: document.body,
  status: document.querySelector("#status"),
  journeys: document.querySelector("#journeys"),
  emptyState: document.querySelector("#empty-state"),
  tableWrap: document.querySelector(".table-wrap"),
  uploadForm: document.querySelector("#upload-form"),
  emailFile: document.querySelector("#email-file"),
  fileLabel: document.querySelector("#file-label span"),
  importButton: document.querySelector("#import-button"),
  periodYear: document.querySelector("#period-year"),
  periodMonth: document.querySelector("#period-month"),
  clearPeriod: document.querySelector("#clear-period"),
  matchingPanel: document.querySelector("#matching-panel"),
  matchingJobs: document.querySelector("#matching-jobs"),
  selectedRoute: document.querySelector("#selected-route"),
  mapReset: document.querySelector("#map-reset"),
  spendChart: document.querySelector("#spend-chart"),
  modeChart: document.querySelector("#mode-chart"),
  modeEmpty: document.querySelector("#mode-empty"),
  ledgerSearch: document.querySelector("#ledger-search"),
  ledgerStatus: document.querySelector("#ledger-status"),
  ledgerMode: document.querySelector("#ledger-mode"),
  ledgerSort: document.querySelector("#ledger-sort"),
  ledgerDirection: document.querySelector("#ledger-direction"),
  clearLedgerFilters: document.querySelector("#clear-ledger-filters"),
  emptyReset: document.querySelector("#empty-reset"),
  inboundAPIStatus: document.querySelector("#inbound-api-status"),
  ingestionAuditStatus: document.querySelector("#ingestion-audit-status"),
  journeyDialog: document.querySelector("#journey-dialog"),
  journeyForm: document.querySelector("#journey-form"),
};

const monthLabels = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const activeRouteStatuses = new Set(["queued", "matching"]);

let journeyData = [];
let visibleJourneys = [];
let selectedYear = null;
let selectedMonth = "all";
let selectedJourneyId = null;
let refreshTimer = null;
let statusTimer = null;
let loadingJourneys = false;
let hasFitMap = false;
let dataFingerprint = "";
const ledgerState = {
  query: "",
  status: "all",
  mode: "all",
  sortKey: "date",
  sortDirection: "desc",
};

// Render caches: rows, map layers, and chart nodes persist between renders so
// the 1s route-progress poll only touches what actually changed.
const rowCache = new Map();
const mapCache = new Map();
const jobCache = new Map();
const distanceCache = new Map();
const spendColumns = [];
const modeRows = new Map();

const map = L.map("map", { zoomControl: true }).setView([46.8, 8.25], 8);
L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
  maxZoom: 19,
  attribution: "&copy; OpenStreetMap contributors",
  className: "base-map-tiles",
}).addTo(map);

map.createPane("routeGlow");
map.getPane("routeGlow").style.zIndex = 440;
map.getPane("routeGlow").style.pointerEvents = "none";
map.createPane("routeCore");
map.getPane("routeCore").style.zIndex = 445;

const routeGlowLayer = L.featureGroup().addTo(map);
const routeLayer = L.featureGroup().addTo(map);

elements.emailFile.addEventListener("change", () => {
  const file = elements.emailFile.files[0];
  elements.fileLabel.textContent = file ? file.name : "Choose receipt";
  elements.fileLabel.parentElement.title = file ? file.name : "Choose an SBB email receipt";
});

elements.uploadForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const file = elements.emailFile.files[0];
  if (!file) return;

  setImporting(true);
  setStatus(`Reading ${file.name}...`, "working");
  const body = new FormData();
  body.set("email", file);
  try {
    const response = await fetch("/api/import", { method: "POST", body });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Import failed");

    const importedJourneys = Array.isArray(result.journeys) ? result.journeys : [result.journey].filter(Boolean);
    if (result.created) {
      for (const journey of importedJourneys) upsertJourney(journey);
      selectedYear = journeyYear(importedJourneys[0]);
      selectedMonth = journeyMonth(importedJourneys[0]);
      renderDashboard({ fitMap: true });
      const count = result.createdCount || importedJourneys.length;
      setStatus(`${count} ${count === 1 ? "journey" : "journeys"} imported. Route reconstruction has started.`, "success");
    } else {
      setStatus("This receipt was already in your journey log.", "success");
    }
    elements.emailFile.value = "";
    elements.fileLabel.textContent = "Choose receipt";
    await loadJourneys();
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    setImporting(false);
  }
});

elements.mapReset.addEventListener("click", () => {
  selectedJourneyId = null;
  updateSelection();
  fitAllRoutes();
});

elements.periodYear.addEventListener("change", () => {
  selectedYear = elements.periodYear.value;
  ensureSelectedPeriod();
  selectedJourneyId = null;
  hasFitMap = false;
  renderDashboard({ fitMap: true });
});
elements.periodMonth.addEventListener("change", () => {
  selectedMonth = elements.periodMonth.value;
  selectedJourneyId = null;
  hasFitMap = false;
  renderDashboard({ fitMap: true });
});
elements.clearPeriod.addEventListener("click", () => {
  selectedYear = "all";
  selectedMonth = "all";
  selectedJourneyId = null;
  hasFitMap = false;
  renderDashboard({ fitMap: true });
});

elements.ledgerSearch.addEventListener("input", () => {
  ledgerState.query = elements.ledgerSearch.value.trim().toLocaleLowerCase("de-CH");
  renderJourneyLedger(visibleJourneys);
});
elements.ledgerSearch.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && elements.ledgerSearch.value) {
    event.stopPropagation();
    elements.ledgerSearch.value = "";
    ledgerState.query = "";
    renderJourneyLedger(visibleJourneys);
  }
});
elements.ledgerStatus.addEventListener("change", () => {
  ledgerState.status = elements.ledgerStatus.value;
  renderJourneyLedger(visibleJourneys);
});
elements.ledgerMode.addEventListener("change", () => {
  ledgerState.mode = elements.ledgerMode.value;
  renderJourneyLedger(visibleJourneys);
});
elements.ledgerSort.addEventListener("change", () => {
  ledgerState.sortKey = elements.ledgerSort.value;
  renderJourneyLedger(visibleJourneys);
});
elements.ledgerDirection.addEventListener("click", () => {
  ledgerState.sortDirection = ledgerState.sortDirection === "asc" ? "desc" : "asc";
  renderJourneyLedger(visibleJourneys);
});
elements.clearLedgerFilters.addEventListener("click", clearLedgerFilters);
elements.emptyReset.addEventListener("click", clearLedgerFilters);
elements.journeyForm.addEventListener("submit", saveJourneyChanges);
document.querySelector("#journey-dialog-close").addEventListener("click", closeJourneyDialog);
document.querySelector("#journey-cancel").addEventListener("click", closeJourneyDialog);
document.querySelector("#journey-delete").addEventListener("click", deleteSelectedJourney);
for (const button of document.querySelectorAll(".sort-button")) {
  button.addEventListener("click", () => {
    const key = button.dataset.sortKey;
    ledgerState.sortDirection = ledgerState.sortKey === key
      ? (ledgerState.sortDirection === "asc" ? "desc" : "asc")
      : defaultSortDirection(key);
    ledgerState.sortKey = key;
    renderJourneyLedger(visibleJourneys);
  });
}

document.addEventListener("keydown", (event) => {
  if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) return;
  const target = event.target;
  if (target instanceof Element && target.closest("input, select, textarea, dialog")) return;
  event.preventDefault();
  elements.ledgerSearch.focus();
  elements.ledgerSearch.select();
});

async function loadIngestionStatus() {
  try {
    const response = await fetch("/api/health");
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Could not load ingestion status");
    const configured = result.inboundEmailConfigured === true;
    elements.inboundAPIStatus.textContent = configured ? "enabled" : "disabled (set INBOUND_TOKEN)";
    elements.inboundAPIStatus.dataset.state = configured ? "ready" : "disabled";
    const auditResponse = await fetch("/api/ingestions");
    const audit = await auditResponse.json();
    if (!auditResponse.ok) throw new Error(audit.error || "Could not load import audit");
    const rejected = (audit.summary?.rejected || 0) + (audit.summary?.failed || 0);
    elements.ingestionAuditStatus.textContent = `${audit.summary?.journeys || 0} imported, ${rejected} need review`;
    elements.ingestionAuditStatus.dataset.state = rejected > 0 ? "warning" : "ready";
  } catch (_error) {
    elements.inboundAPIStatus.textContent = "status unavailable";
    elements.inboundAPIStatus.dataset.state = "disabled";
    elements.ingestionAuditStatus.textContent = "import audit unavailable";
    elements.ingestionAuditStatus.dataset.state = "warning";
  }
}

async function loadJourneys() {
  if (loadingJourneys) return;
  loadingJourneys = true;
  try {
    const previous = new Map(journeyData.map((journey) => [journey.id, journey.routeStatus]));
    const response = await fetch("/api/journeys");
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Could not load journeys");
    const fingerprint = result.journeys
      .map((journey) => `${journey.id}|${journey.updatedAt}|${journey.routeProgress}|${journey.routeProgressMessage || ""}`)
      .join("\n");
    if (fingerprint !== dataFingerprint || !hasFitMap) {
      dataFingerprint = fingerprint;
      journeyData = result.journeys;
      ensureSelectedPeriod();
      renderDashboard({ fitMap: !hasFitMap });
    } else {
      scheduleRefresh(journeyData.some((journey) => activeRouteStatuses.has(journey.routeStatus)));
    }

    const completed = journeyData.find((journey) => {
      const prior = previous.get(journey.id);
      return activeRouteStatuses.has(prior) && !activeRouteStatuses.has(journey.routeStatus);
    });
    if (completed) {
      if (completed.routeStatus === "error") {
        setStatus(`Route reconstruction failed for ${completed.originName} to ${completed.destinationName}.`, "error");
      } else {
        setStatus(`Route ready: ${completed.originName} to ${completed.destinationName}.`, "success");
      }
    }
  } catch (error) {
    setStatus(error.message, "error");
  } finally {
    loadingJourneys = false;
    elements.body.classList.remove("is-loading");
  }
}

function renderDashboard(options = {}) {
  visibleJourneys = filteredJourneys();
  renderPeriodFilters();
  renderMetrics(visibleJourneys);
  updateSpendChart(yearFilteredJourneys());
  updateModeChart(visibleJourneys);
  renderMatchingPanel(visibleJourneys);
  renderMap(visibleJourneys, options.fitMap === true);
  renderJourneyLedger(visibleJourneys);
  scheduleRefresh(journeyData.some((journey) => activeRouteStatuses.has(journey.routeStatus)));
}

function ensureSelectedPeriod() {
  const years = availableYears();
  if (selectedYear !== "all" && (selectedYear === null || !years.includes(selectedYear))) {
    selectedYear = years[0] || "all";
  }
  const months = availableMonths();
  if (selectedMonth !== "all" && !months.includes(selectedMonth)) selectedMonth = "all";
}

function availableYears() {
  return [...new Set(journeyData.map(journeyYear).filter(Boolean))].sort((left, right) => right.localeCompare(left));
}

function availableMonths() {
  return monthLabels.map((_label, index) => String(index + 1).padStart(2, "0"));
}

function renderPeriodFilters() {
  const years = availableYears();
  setSelectOptions(elements.periodYear, [
    { value: "all", label: "All years" },
    ...years.map((year) => ({ value: year, label: year })),
  ], selectedYear);
  setSelectOptions(elements.periodMonth, [
    { value: "all", label: "All months" },
    ...availableMonths().map((month) => ({ value: month, label: monthLabels[Number(month) - 1] })),
  ], selectedMonth);
  elements.clearPeriod.disabled = selectedYear === "all" && selectedMonth === "all";
  document.querySelector("#period-label").textContent = periodLabel();
}

function setSelectOptions(select, options, selected) {
  const signature = options.map((option) => `${option.value}:${option.label}`).join("|");
  if (select.dataset.options !== signature) {
    select.replaceChildren(...options.map((option) => {
      const element = document.createElement("option");
      element.value = option.value;
      element.textContent = option.label;
      return element;
    }));
    select.dataset.options = signature;
  }
  select.value = selected;
}

function periodLabel() {
  const month = selectedMonth === "all" ? "" : monthLabels[Number(selectedMonth) - 1];
  if (selectedYear === "all" && !month) return "All time";
  if (selectedYear === "all") return `${month}, all years`;
  if (!month) return selectedYear;
  return `${month} ${selectedYear}`;
}

function filteredJourneys() {
  return yearFilteredJourneys().filter((journey) => selectedMonth === "all" || journeyMonth(journey) === selectedMonth);
}

function yearFilteredJourneys() {
  if (selectedYear === "all") return journeyData;
  return journeyData.filter((journey) => journeyYear(journey) === selectedYear);
}

function renderMetrics(journeys) {
  const confirmedJourneys = journeys.filter(isConfirmedJourney);
  const total = journeys.reduce((sum, journey) => sum + journey.priceCents, 0);
  const distance = confirmedJourneys.reduce((sum, journey) => sum + journeyDistanceKm(journey), 0);
  const matched = confirmedJourneys.filter((journey) => journey.routeGeojson).length;
  const active = journeys.filter((journey) => activeRouteStatuses.has(journey.routeStatus)).length;
  const average = journeys.length > 0 ? Math.round(total / journeys.length) : 0;
  const coverage = confirmedJourneys.length > 0 ? Math.round((matched / confirmedJourneys.length) * 100) : 0;

  setOutput("#total-spend", formatCHF(total));
  setOutput("#journey-count", String(confirmedJourneys.length));
  setOutput("#distance-total", formatDistance(distance));
  setOutput("#average-ticket", formatCHF(average));
  setOutput("#route-coverage", `${coverage}%`);
  setOutput("#matching-count", String(active));
  setOutput("#matched-count", String(matched));
  setOutput("#unmatched-count", String(Math.max(0, confirmedJourneys.length - matched)));

  const spendContext = distance > 0
    ? `${formatCHF(Math.round(total / distance))} per tracked km`
    : "All imported tickets";
  document.querySelector("#spend-context").textContent = spendContext;
  document.querySelector(".processing-metric").classList.toggle("is-active", active > 0);
}

function buildSpendChart() {
  monthLabels.forEach((label, index) => {
    const column = document.createElement("button");
    column.type = "button";
    column.className = "month-column";
    column.addEventListener("click", () => selectMonth(index));

    const shell = document.createElement("div");
    shell.className = "month-bar-shell";
    const bar = document.createElement("i");
    bar.className = "month-bar";
    const amount = document.createElement("span");
    amount.className = "month-value";
    shell.append(bar, amount);

    const monthLabel = document.createElement("span");
    monthLabel.className = "month-label";
    monthLabel.textContent = label;
    column.append(shell, monthLabel);
    elements.spendChart.append(column);
    spendColumns.push({ column, bar, amount });
  });
}

function updateSpendChart(journeys) {
  const monthly = Array(12).fill(0);
  for (const journey of journeys) {
    const month = Number(journey.journeyDate?.slice(5, 7)) - 1;
    if (month >= 0 && month < 12) monthly[month] += journey.priceCents;
  }
  const maximum = Math.max(...monthly, 0);
  const peakIndex = monthly.indexOf(maximum);
  elements.spendChart.dataset.filtered = String(selectedMonth !== "all");

  spendColumns.forEach((slot, index) => {
    const value = monthly[index];
    const height = maximum > 0 ? Math.max(1.5, (value / maximum) * 100) : 0;
    slot.column.setAttribute("aria-pressed", String(selectedMonth === String(index + 1).padStart(2, "0")));
    slot.column.setAttribute("aria-label", `Filter by ${monthLabels[index]}, ${formatCHF(value)}`);
    slot.column.classList.toggle("is-peak", value > 0 && index === peakIndex);
    slot.column.style.setProperty("--bar-height", `${height}%`);
    slot.bar.style.height = `${height}%`;
    slot.amount.textContent = formatCHF(value);
  });

  if (selectedMonth !== "all") {
    const selectedIndex = Number(selectedMonth) - 1;
    document.querySelector("#highest-month").textContent = `${monthLabels[selectedIndex]} · ${formatCHF(monthly[selectedIndex])}`;
  } else {
    document.querySelector("#highest-month").textContent = maximum > 0
      ? `${monthLabels[peakIndex]} peak · ${formatCHF(maximum)}`
      : "No spend yet";
  }
}

function selectMonth(index) {
  const month = String(index + 1).padStart(2, "0");
  selectedMonth = selectedMonth === month ? "all" : month;
  selectedJourneyId = null;
  hasFitMap = false;
  renderDashboard({ fitMap: true });
}

function buildModeChart() {
  const labels = { rail: "Rail", bus: "Bus", tram: "Tram", other: "Other" };
  for (const mode of ["rail", "bus", "tram", "other"]) {
    const row = document.createElement("div");
    row.className = "mode-row";
    row.dataset.mode = mode;
    row.hidden = true;
    const label = document.createElement("strong");
    label.textContent = labels[mode];
    const track = document.createElement("span");
    track.className = "mode-track";
    const fill = document.createElement("i");
    track.append(fill);
    const value = document.createElement("output");
    row.append(label, track, value);
    elements.modeChart.append(row);
    modeRows.set(mode, { row, fill, value });
  }
}

function updateModeChart(journeys) {
  const totals = { rail: 0, bus: 0, tram: 0, other: 0 };
  let travelMinutes = 0;
  for (const journey of journeys) {
    if (!isConfirmedJourney(journey)) continue;
    travelMinutes += journeyDurationMinutes(journey);
    forEachRouteLine(journey.routeGeojson, (coordinates, properties) => {
      totals[modeGroup(properties?.mode)] += lineDistanceKm(coordinates);
    });
  }
  const totalDistance = Object.values(totals).reduce((sum, value) => sum + value, 0);
  elements.modeEmpty.hidden = totalDistance > 0;

  for (const [mode, slot] of modeRows) {
    const visible = totalDistance > 0 && totals[mode] > 0;
    slot.row.hidden = !visible;
    if (visible) {
      slot.fill.style.width = `${(totals[mode] / totalDistance) * 100}%`;
      slot.value.textContent = formatDistance(totals[mode]);
    }
  }
  document.querySelector("#travel-time").textContent = formatTravelTime(travelMinutes);
}

function renderMatchingPanel(journeys) {
  const active = journeys.filter((journey) => activeRouteStatuses.has(journey.routeStatus));
  elements.matchingPanel.hidden = active.length === 0;
  setOutput("#active-route-count", String(active.length));

  const seen = new Set();
  for (const journey of active) {
    seen.add(journey.id);
    let job = jobCache.get(journey.id);
    if (!job) {
      const element = document.createElement("div");
      element.className = "matching-job";
      const route = document.createElement("strong");
      route.textContent = `${journey.originName} → ${journey.destinationName}`;
      const phase = document.createElement("span");
      const progress = progressTrack(0);
      element.append(route, phase, progress);
      elements.matchingJobs.append(element);
      job = { element, route, phase, fill: progress.querySelector("i"), track: progress };
      jobCache.set(journey.id, job);
    }
    job.route.textContent = `${journey.originName} → ${journey.destinationName}`;
    job.phase.textContent = journey.routeProgressMessage || "Waiting for route matcher";
    const percent = journey.routeStatus === "queued" ? 3 : journey.routeProgress || 5;
    job.fill.style.width = `${Math.max(0, Math.min(100, percent))}%`;
    job.track.setAttribute("aria-valuenow", String(Math.round(percent)));
  }
  for (const [id, job] of jobCache) {
    if (!seen.has(id)) {
      job.element.remove();
      jobCache.delete(id);
    }
  }
}

function mapSignature(journey) {
  return `${journey.updatedAt}|${journey.occurrenceStatus}|${journey.routeStatus}`;
}

function renderMap(journeys, fitMap) {
  const wanted = new Map();
  for (const journey of journeys) {
    if (!isConfirmedJourney(journey) || !journey.routeGeojson) continue;
    wanted.set(journey.id, journey);
  }
  if (selectedJourneyId && !wanted.has(selectedJourneyId)) selectedJourneyId = null;

  for (const [id, entry] of mapCache) {
    const journey = wanted.get(id);
    if (journey && mapSignature(journey) === entry.sig) continue;
    routeGlowLayer.removeLayer(entry.glow);
    routeLayer.removeLayer(entry.core);
    mapCache.delete(id);
  }

  for (const [id, journey] of wanted) {
    if (mapCache.has(id)) continue;
    const glow = L.geoJSON(journey.routeGeojson, {
      pane: "routeGlow",
      style: glowStyle(false),
    }).addTo(routeGlowLayer);
    const core = L.geoJSON(journey.routeGeojson, {
      pane: "routeCore",
      style: coreStyle(false),
    }).addTo(routeLayer);
    const tooltip = document.createElement("span");
    tooltip.textContent = `${journey.originName} → ${journey.destinationName}`;
    core.bindTooltip(tooltip, { sticky: true });
    core.on("click", () => selectJourney(id, true));
    mapCache.set(id, { glow, core, sig: mapSignature(journey) });
  }

  updateSelection();
  if (fitMap) fitAllRoutes();
}

function glowStyle(selected) {
  return {
    color: selected ? "#df3f32" : "#f05a47",
    weight: selected ? 22 : 15,
    opacity: selected ? 0.2 : 0.09,
    lineCap: "round",
    lineJoin: "round",
    className: "journey-glow",
  };
}

function coreStyle(selected) {
  return {
    color: selected ? "#a92822" : "#df3f32",
    weight: selected ? 7 : 4.5,
    opacity: selected ? 0.92 : 0.43,
    lineCap: "round",
    lineJoin: "round",
    className: "journey-track",
  };
}

function fitAllRoutes() {
  if (routeLayer.getLayers().length === 0) return;
  const bounds = routeLayer.getBounds();
  if (!bounds.isValid()) return;
  map.fitBounds(bounds, { padding: [30, 30], maxZoom: 13 });
  hasFitMap = true;
}

function selectJourney(id, fitRoute) {
  selectedJourneyId = id;
  updateSelection();
  if (fitRoute) {
    const layer = mapCache.get(id)?.core;
    if (layer && layer.getBounds().isValid()) {
      map.fitBounds(layer.getBounds(), { padding: [42, 42], maxZoom: 14 });
    }
  }
}

function updateSelection() {
  for (const [id, entry] of mapCache) {
    const selected = id === selectedJourneyId;
    entry.glow.setStyle(glowStyle(selected));
    entry.core.setStyle(coreStyle(selected));
    if (selected) entry.core.bringToFront();
  }
  for (const row of elements.journeys.querySelectorAll("tr")) {
    row.classList.toggle("is-selected", row.dataset.journeyId === selectedJourneyId);
  }

  const journey = journeyData.find((candidate) => candidate.id === selectedJourneyId);
  elements.selectedRoute.hidden = !journey;
  elements.mapReset.hidden = mapCache.size === 0;
  if (!journey) {
    elements.selectedRoute.replaceChildren();
    return;
  }
  const route = document.createElement("strong");
  route.textContent = `${journey.originName} → ${journey.destinationName}`;
  const detail = document.createElement("span");
  const timing = reliableJourneyTime(journey)
    ? `${clock(journey.validFrom)}–${clock(journey.validTo)}`
    : "time not confirmed";
  detail.textContent = `${formatDate(journey.journeyDate)} · ${timing} · ${formatDistance(journeyDistanceKm(journey))} · ${formatCHF(journey.priceCents)}`;
  elements.selectedRoute.replaceChildren(route, detail);
}

function rowSignature(journey) {
  return [
    journey.journeyDate, journey.originName, journey.destinationName,
    journey.validFrom, journey.validTo, journey.timeSource, journey.occurrenceStatus,
    journey.fareProduct, journey.fareReduction, journey.travelClass, journey.priceCents,
    journey.routeStatus, journey.routeSummary, journey.routeConfidence,
    journey.routeCacheHits, journey.routeDataVersion, journey.routeProvider,
    journeyDistanceKm(journey).toFixed(2),
  ].join("|");
}

function renderJourneyLedger(journeys) {
  const ledgerJourneys = filterAndSortJourneys(journeys);
  const hasFilters = ledgerFiltersActive();
  elements.emptyState.hidden = ledgerJourneys.length > 0;
  elements.tableWrap.hidden = ledgerJourneys.length === 0;
  document.querySelector("#ledger-result-count").textContent = ledgerJourneys.length === journeys.length
    ? `${ledgerJourneys.length} shown`
    : `${ledgerJourneys.length} of ${journeys.length} shown`;
  document.querySelector("#empty-title").textContent = hasFilters ? "No journeys match" : "No journeys in this view";
  document.querySelector("#empty-message").textContent = hasFilters
    ? "Adjust or clear the ledger filters to see more journeys."
    : "Import a supported SBB receipt or choose another period.";
  elements.emptyReset.hidden = !hasFilters;
  elements.clearLedgerFilters.disabled = !hasFilters;
  updateSortControls();

  const seen = new Set();
  ledgerJourneys.forEach((journey, index) => {
    seen.add(journey.id);
    const sig = rowSignature(journey);
    let entry = rowCache.get(journey.id);
    if (!entry || entry.sig !== sig) {
      const built = buildJourneyRow(journey);
      if (entry && entry.row.isConnected) entry.row.replaceWith(built.row);
      entry = { row: built.row, progress: built.progress, sig };
      rowCache.set(journey.id, entry);
    }
    if (entry.progress) patchRowProgress(entry.progress, journey);
    const current = elements.journeys.children[index];
    if (current !== entry.row) elements.journeys.insertBefore(entry.row, current || null);
  });
  for (const [id, entry] of rowCache) {
    if (!seen.has(id)) {
      entry.row.remove();
      rowCache.delete(id);
    }
  }
  updateSelection();
}

function patchRowProgress(progress, journey) {
  const percent = journey.routeStatus === "queued" ? 3 : journey.routeProgress || 5;
  progress.label.textContent = journey.routeStatus === "queued" ? "Queued" : `${journey.routeProgress || 0}% reconstructed`;
  progress.detail.textContent = journey.routeProgressMessage || "Waiting for route matcher";
  progress.fill.style.width = `${Math.max(0, Math.min(100, percent))}%`;
  progress.track.setAttribute("aria-valuenow", String(Math.round(percent)));
}

function buildJourneyRow(journey) {
  const row = document.createElement("tr");
  row.dataset.journeyId = journey.id;
  row.tabIndex = 0;
  row.setAttribute("aria-label", `Show route details for ${journey.originName} to ${journey.destinationName} on the map`);
  row.addEventListener("click", (event) => {
    if (event.target.closest("button")) return;
    revealJourneyFromLedger(journey.id);
  });
  row.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      revealJourneyFromLedger(journey.id);
    }
  });

  const date = tableCell("date-cell", "Date");
  const dateValue = document.createElement("time");
  dateValue.dateTime = journey.journeyDate;
  dateValue.textContent = formatDate(journey.journeyDate);
  date.append(dateValue);

  const route = tableCell("route-cell", "Route");
  const routeName = document.createElement("span");
  routeName.className = "route-name";
  routeName.textContent = `${journey.originName} → ${journey.destinationName}`;
  const routeTime = document.createElement("span");
  routeTime.className = "route-meta";
  routeTime.textContent = reliableJourneyTime(journey)
    ? `${clock(journey.validFrom)}–${clock(journey.validTo)}`
    : "Ticket validity · time unconfirmed";
  route.append(routeName, routeTime);

  const movement = tableCell("movement-cell", "Movement");
  const movementDistance = document.createElement("strong");
  movementDistance.textContent = !isConfirmedJourney(journey)
    ? "Not confirmed"
    : (journey.routeGeojson ? formatDistance(journeyDistanceKm(journey)) : "Not mapped");
  const movementDuration = document.createElement("span");
  movementDuration.textContent = formatDuration(journeyDurationMinutes(journey));
  movement.append(movementDistance, movementDuration);

  const ticket = tableCell("ticket-cell", "Ticket");
  const ticketName = document.createElement("span");
  ticketName.className = "ticket-name";
  ticketName.textContent = journey.fareProduct || "Public transport ticket";
  const ticketDetail = document.createElement("span");
  ticketDetail.className = "ticket-detail";
  ticketDetail.textContent = journey.fareReduction || journey.travelClass || "";
  ticket.append(ticketName, ticketDetail);

  const price = tableCell("price-cell", "Cost");
  const priceValue = document.createElement("span");
  priceValue.className = "price-value";
  priceValue.textContent = formatCHF(journey.priceCents);
  price.append(priceValue);

  const status = routeStatusCell(journey);
  row.append(date, route, movement, ticket, price, status.cell);
  return { row, progress: status.progress };
}

function revealJourneyFromLedger(id) {
  selectJourney(id, true);
  const mapRect = document.querySelector("#map").getBoundingClientRect();
  if (mapRect.bottom < 0 || mapRect.top > window.innerHeight * 0.6) {
    document.querySelector(".map-panel").scrollIntoView({ behavior: "smooth", block: "nearest" });
  }
  map.invalidateSize();
}

function filterAndSortJourneys(journeys) {
  const filtered = journeys.filter((journey) => {
    if (ledgerState.query && !journeySearchText(journey).includes(ledgerState.query)) return false;
    if (ledgerState.status !== "all" && journeyLedgerStatus(journey) !== ledgerState.status) return false;
    if (ledgerState.mode !== "all" && !journeyModes(journey).has(ledgerState.mode)) return false;
    return true;
  });

  const direction = ledgerState.sortDirection === "asc" ? 1 : -1;
  return [...filtered].sort((left, right) => {
    const comparison = compareJourneys(left, right, ledgerState.sortKey);
    if (comparison !== 0) return comparison * direction;
    return compareText(left.id, right.id) * direction;
  });
}

function compareJourneys(left, right, key) {
  if (key === "route") return compareText(`${left.originName} ${left.destinationName}`, `${right.originName} ${right.destinationName}`);
  if (key === "movement") return journeyDistanceKm(left) - journeyDistanceKm(right);
  if (key === "ticket") return compareText(left.fareProduct || "", right.fareProduct || "");
  if (key === "cost") return left.priceCents - right.priceCents;
  if (key === "status") return compareText(journeyLedgerStatus(left), journeyLedgerStatus(right));
  return compareText(`${left.journeyDate || ""} ${left.validFrom || ""}`, `${right.journeyDate || ""} ${right.validFrom || ""}`);
}

function compareText(left, right) {
  return String(left).localeCompare(String(right), "de-CH", { numeric: true, sensitivity: "base" });
}

function journeySearchText(journey) {
  return [
    journey.originName,
    journey.destinationName,
    journey.journeyDate,
    formatDate(journey.journeyDate),
    journey.fareProduct,
    journey.fareReduction,
    journey.travelClass,
    journey.routeProvider,
    journey.routeSummary,
  ].filter(Boolean).join(" ").toLocaleLowerCase("de-CH");
}

function journeyLedgerStatus(journey) {
  if (activeRouteStatuses.has(journey.routeStatus)) return "processing";
  if (journey.routeGeojson) return "mapped";
  if (journey.routeStatus === "error") return "failed";
  return "unmapped";
}

function journeyModes(journey) {
  const modes = new Set();
  forEachRouteLine(journey.routeGeojson, (_coordinates, properties) => {
    modes.add(modeGroup(properties?.mode));
  });
  return modes;
}

function ledgerFiltersActive() {
  return Boolean(ledgerState.query || ledgerState.status !== "all" || ledgerState.mode !== "all");
}

function clearLedgerFilters() {
  ledgerState.query = "";
  ledgerState.status = "all";
  ledgerState.mode = "all";
  elements.ledgerSearch.value = "";
  elements.ledgerStatus.value = "all";
  elements.ledgerMode.value = "all";
  renderJourneyLedger(visibleJourneys);
  elements.ledgerSearch.focus();
}

function defaultSortDirection(key) {
  return ["date", "movement", "cost"].includes(key) ? "desc" : "asc";
}

function updateSortControls() {
  elements.ledgerSort.value = ledgerState.sortKey;
  const descending = ledgerState.sortDirection === "desc";
  elements.ledgerDirection.querySelector("span").textContent = descending ? "↓" : "↑";
  const directionAction = descending ? "Change to ascending order" : "Change to descending order";
  elements.ledgerDirection.setAttribute("aria-label", directionAction);
  elements.ledgerDirection.title = directionAction;

  for (const heading of document.querySelectorAll("th[data-sort-key]")) {
    const selected = heading.dataset.sortKey === ledgerState.sortKey;
    heading.setAttribute("aria-sort", selected ? (descending ? "descending" : "ascending") : "none");
    const icon = heading.querySelector(".sort-button span");
    icon.textContent = selected ? (descending ? "↓" : "↑") : "↕";
  }
}

function routeStatusCell(journey) {
  const cell = tableCell("status-cell", "Status");
  const line = document.createElement("div");
  line.className = "status-line";
  const symbol = document.createElement("i");
  symbol.className = "status-symbol";
  const label = document.createElement("span");
  label.className = "status-label";
  line.append(symbol, label);
  cell.append(line);

  const actions = document.createElement("div");
  actions.className = "row-actions";

  if (activeRouteStatuses.has(journey.routeStatus)) {
    symbol.classList.add("is-active");
    const detail = statusDetail("");
    const track = progressTrack(0, "row-progress");
    cell.append(detail, track);
    actions.append(editJourneyButton(journey));
    cell.append(actions);
    return { cell, progress: { label, detail, fill: track.querySelector("i"), track } };
  }

  if (journey.routeGeojson) {
    symbol.classList.add("is-matched");
    label.textContent = journey.occurrenceStatus === "unconfirmed"
      ? "Planned route · trip unconfirmed"
      : journey.routeStatus === "estimated"
        ? "Mapped · historical estimate"
        : (journey.routeStatus === "ambiguous" ? "Mapped · check timing" : "Mapped");
    let detailText = journey.routeSummary || `${Math.round(journey.routeConfidence * 100)}% confidence`;
    if (journey.routeCacheHits) detailText += ` · ${journey.routeCacheHits} cached`;
    if (journey.routeStatus === "estimated" && journey.routeDataVersion) detailText += ` · ${journey.routeDataVersion}`;
    cell.append(statusDetail(detailText));
    actions.append(routeButton(journey, journey.routeProvider?.includes("stop sequence") ? "Trace network" : "Rebuild route"));
  } else if (journey.routeStatus === "error") {
    symbol.classList.add("is-error");
    label.textContent = "Route failed";
    cell.append(statusDetail(journey.routeSummary || "The route provider could not reconstruct this journey."));
    actions.append(routeButton(journey, "Retry route"));
  } else {
    label.textContent = "Not mapped";
    actions.append(routeButton(journey, "Match route"));
  }
  actions.append(editJourneyButton(journey));
  cell.append(actions);
  return { cell, progress: null };
}

function editJourneyButton(journey) {
  const button = document.createElement("button");
  button.className = "match-button";
  button.type = "button";
  button.textContent = journey.occurrenceStatus === "unconfirmed" ? "Review ticket" : "Edit";
  button.addEventListener("click", (event) => {
    event.stopPropagation();
    const current = journeyData.find((candidate) => candidate.id === journey.id);
    if (current) openJourneyDialog(current);
  });
  return button;
}

function openJourneyDialog(journey) {
  document.querySelector("#journey-id").value = journey.id;
  document.querySelector("#journey-date").value = journey.journeyDate || "";
  document.querySelector("#journey-origin").value = journey.originName || "";
  document.querySelector("#journey-destination").value = journey.destinationName || "";
  document.querySelector("#journey-valid-from").value = localDateTimeValue(journey.validFrom);
  document.querySelector("#journey-valid-to").value = localDateTimeValue(journey.validTo);
  document.querySelector("#journey-occurrence").value = journey.occurrenceStatus || "unconfirmed";
  document.querySelector("#journey-time-source").value = journey.timeSource || "validity_window";
  document.querySelector("#journey-notes").value = journey.notes || "";
  elements.journeyDialog.showModal();
}

function closeJourneyDialog() {
  elements.journeyDialog.close();
}

async function saveJourneyChanges(event) {
  event.preventDefault();
  const id = document.querySelector("#journey-id").value;
  const body = {
    journeyDate: document.querySelector("#journey-date").value,
    originName: document.querySelector("#journey-origin").value,
    destinationName: document.querySelector("#journey-destination").value,
    validFrom: zurichRFC3339(document.querySelector("#journey-valid-from").value),
    validTo: zurichRFC3339(document.querySelector("#journey-valid-to").value),
    occurrenceStatus: document.querySelector("#journey-occurrence").value,
    timeSource: document.querySelector("#journey-time-source").value,
    notes: document.querySelector("#journey-notes").value,
  };
  try {
    const response = await fetch(`/api/journeys/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Could not update journey");
    upsertJourney(result.journey);
    closeJourneyDialog();
    renderDashboard({ fitMap: true });
    setStatus("Journey updated. Route reconstruction has been queued.", "success");
  } catch (error) {
    setStatus(error.message, "error");
  }
}

async function deleteSelectedJourney() {
  const id = document.querySelector("#journey-id").value;
  const journey = journeyData.find((candidate) => candidate.id === id);
  if (!journey || !window.confirm(`Delete ${journey.originName} to ${journey.destinationName}? This cannot be undone.`)) return;
  try {
    const response = await fetch(`/api/journeys/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "X-Billett-Confirm-Delete": "delete" },
    });
    if (!response.ok) {
      const result = await response.json();
      throw new Error(result.error || "Could not delete journey");
    }
    journeyData = journeyData.filter((candidate) => candidate.id !== id);
    selectedJourneyId = null;
    closeJourneyDialog();
    renderDashboard({ fitMap: true });
    setStatus("Journey deleted.", "success");
  } catch (error) {
    setStatus(error.message, "error");
  }
}

function routeButton(journey, label) {
  const button = document.createElement("button");
  button.className = "match-button";
  button.type = "button";
  button.textContent = label;
  button.addEventListener("click", (event) => {
    event.stopPropagation();
    const current = journeyData.find((candidate) => candidate.id === journey.id);
    if (current) matchRoute(current);
  });
  return button;
}

async function matchRoute(journey) {
  updateJourney(journey.id, {
    routeStatus: "queued",
    routeProgress: 0,
    routeProgressMessage: "Waiting for route matcher",
  });
  renderDashboard();
  setStatus(`Queued ${journey.originName} to ${journey.destinationName}.`, "working");
  try {
    const response = await fetch(`/api/journeys/${encodeURIComponent(journey.id)}/match-route`, { method: "POST" });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "Route matching failed");
    upsertJourney(result.journey);
    renderDashboard();
    setStatus("Route reconstruction started in the background.", "success");
    await loadJourneys();
  } catch (error) {
    setStatus(error.message, "error");
    await loadJourneys();
  }
}

function updateJourney(id, patch) {
  journeyData = journeyData.map((journey) => journey.id === id ? { ...journey, ...patch } : journey);
}

function upsertJourney(next) {
  const found = journeyData.some((journey) => journey.id === next.id);
  journeyData = found
    ? journeyData.map((journey) => journey.id === next.id ? next : journey)
    : [next, ...journeyData];
}

function scheduleRefresh(active) {
  if (active && refreshTimer === null) {
    refreshTimer = window.setInterval(loadJourneys, 1000);
    return;
  }
  if (!active && refreshTimer !== null) {
    window.clearInterval(refreshTimer);
    refreshTimer = null;
  }
}

function setImporting(active) {
  elements.importButton.disabled = active;
  elements.importButton.setAttribute("aria-busy", String(active));
  elements.importButton.querySelector("span").textContent = active ? "Importing" : "Import";
}

function setStatus(message, tone = "info") {
  window.clearTimeout(statusTimer);
  elements.status.textContent = message;
  elements.status.dataset.tone = tone;
  elements.status.classList.add("is-visible");
  statusTimer = window.setTimeout(() => {
    elements.status.classList.remove("is-visible");
  }, tone === "error" ? 8000 : 4200);
}

function progressTrack(percent, className = "progress-track") {
  const track = document.createElement("div");
  track.className = className;
  track.setAttribute("role", "progressbar");
  track.setAttribute("aria-valuemin", "0");
  track.setAttribute("aria-valuemax", "100");
  track.setAttribute("aria-valuenow", String(Math.round(percent)));
  const fill = document.createElement("i");
  fill.style.width = `${Math.max(0, Math.min(100, percent))}%`;
  track.append(fill);
  return track;
}

function statusDetail(value) {
  const detail = document.createElement("span");
  detail.className = "status-detail";
  detail.textContent = value;
  return detail;
}

function tableCell(className, label) {
  const cell = document.createElement("td");
  cell.className = className;
  cell.dataset.label = label;
  return cell;
}

function setOutput(selector, value) {
  document.querySelector(selector).textContent = value;
}

function journeyYear(journey) {
  return journey.journeyDate?.slice(0, 4) || "";
}

function journeyMonth(journey) {
  return journey.journeyDate?.slice(5, 7) || "";
}

function journeyDurationMinutes(journey) {
  if (!isConfirmedJourney(journey) || !reliableJourneyTime(journey)) return 0;
  const start = new Date(journey.validFrom).getTime();
  const end = new Date(journey.validTo).getTime();
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return 0;
  return Math.round((end - start) / 60000);
}

function isConfirmedJourney(journey) {
  return journey.occurrenceStatus === "confirmed";
}

function reliableJourneyTime(journey) {
  return journey.timeSource === "recorded" || journey.timeSource === "user_supplied";
}

function localDateTimeValue(value) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) return "";
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone: "Europe/Zurich",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).formatToParts(parsed);
  const fields = Object.fromEntries(parts.map((part) => [part.type, part.value]));
  return `${fields.year}-${fields.month}-${fields.day}T${fields.hour}:${fields.minute}`;
}

function zurichRFC3339(value) {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(value)) return "";
  const wallClockAsUTC = Date.parse(`${value}:00Z`);
  let instant = wallClockAsUTC;
  for (let attempt = 0; attempt < 2; attempt += 1) {
    const offset = zurichOffsetMinutes(new Date(instant));
    instant = wallClockAsUTC - offset * 60_000;
  }
  return new Date(instant).toISOString();
}

function zurichOffsetMinutes(instant) {
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone: "Europe/Zurich",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hourCycle: "h23",
  }).formatToParts(instant);
  const fields = Object.fromEntries(parts.map((part) => [part.type, part.value]));
  const represented = Date.UTC(
    Number(fields.year),
    Number(fields.month) - 1,
    Number(fields.day),
    Number(fields.hour),
    Number(fields.minute),
    Number(fields.second),
  );
  return Math.round((represented - instant.getTime()) / 60_000);
}

function journeyDistanceKm(journey) {
  if (!journey.routeGeojson) return 0;
  const cached = distanceCache.get(journey.id);
  if (cached && cached.updatedAt === journey.updatedAt) return cached.km;
  let total = 0;
  forEachRouteLine(journey.routeGeojson, (coordinates) => {
    total += lineDistanceKm(coordinates);
  });
  distanceCache.set(journey.id, { updatedAt: journey.updatedAt, km: total });
  return total;
}

function forEachRouteLine(geojson, callback) {
  if (!geojson) return;
  const features = geojson.type === "FeatureCollection" ? geojson.features || [] : [geojson];
  for (const feature of features) {
    const geometry = feature.geometry || feature;
    if (geometry.type === "LineString") {
      callback(geometry.coordinates || [], feature.properties || {});
    } else if (geometry.type === "MultiLineString") {
      for (const line of geometry.coordinates || []) callback(line, feature.properties || {});
    }
  }
}

function lineDistanceKm(coordinates) {
  let metres = 0;
  for (let index = 1; index < coordinates.length; index += 1) {
    metres += haversineMetres(coordinates[index - 1], coordinates[index]);
  }
  return metres / 1000;
}

function haversineMetres(left, right) {
  if (!left || !right || left.length < 2 || right.length < 2) return 0;
  const radius = 6371000;
  const latitude1 = left[1] * Math.PI / 180;
  const latitude2 = right[1] * Math.PI / 180;
  const latitudeDelta = (right[1] - left[1]) * Math.PI / 180;
  const longitudeDelta = (right[0] - left[0]) * Math.PI / 180;
  const value = Math.sin(latitudeDelta / 2) ** 2
    + Math.cos(latitude1) * Math.cos(latitude2) * Math.sin(longitudeDelta / 2) ** 2;
  return radius * 2 * Math.atan2(Math.sqrt(value), Math.sqrt(1 - value));
}

function modeGroup(value = "") {
  const mode = String(value).trim().toUpperCase();
  if (mode === "B" || mode === "BUS" || mode.includes("BUS") || mode.startsWith("NF")) return "bus";
  if (mode === "T" || mode === "TRAM" || mode.includes("TRAM")) return "tram";
  if (!mode || mode === "WALK" || mode.includes("SHIP") || mode === "BAT" || mode.includes("FUN") || mode.includes("CABLE")) return "other";
  return "rail";
}

function formatCHF(cents) {
  return new Intl.NumberFormat("de-CH", {
    style: "currency",
    currency: "CHF",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(cents / 100);
}

function formatDistance(kilometres) {
  if (!Number.isFinite(kilometres) || kilometres <= 0) return "0 km";
  const digits = kilometres < 10 ? 1 : 0;
  return `${new Intl.NumberFormat("de-CH", { maximumFractionDigits: digits }).format(kilometres)} km`;
}

function formatDate(value) {
  if (!value) return "Unknown";
  const [year, month, day] = value.split("-");
  return `${day}.${month}.${year}`;
}

function clock(value) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) return "--:--";
  return parsed.toLocaleTimeString("de-CH", {
    hour: "2-digit",
    minute: "2-digit",
    timeZone: "Europe/Zurich",
  });
}

function formatDuration(minutes) {
  if (minutes <= 0) return "Duration unknown";
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return remainder ? `${hours} h ${remainder} min` : `${hours} h`;
}

function formatTravelTime(minutes) {
  if (minutes <= 0) return "0 h tracked";
  if (minutes < 60) return `${minutes} min tracked`;
  return `${new Intl.NumberFormat("de-CH", { maximumFractionDigits: 1 }).format(minutes / 60)} h tracked`;
}

buildSpendChart();
buildModeChart();
loadIngestionStatus();
loadJourneys();
