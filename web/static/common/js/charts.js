/* SVG 图表模块：只负责绘制与交互，行情状态和接口请求由 app.js / request.js 管理。 */
(function (window) {
// 按价格在有序执行价数组中的位置插值，供零 Gamma 与关键位图表共用。
function strikePosition(points, strike) {
  if (!points.length || strike < points[0].strike || strike > points[points.length - 1].strike) return null;
  for (let index = 1; index < points.length; index += 1) {
    if (strike <= points[index].strike) {
      const distance = points[index].strike - points[index - 1].strike;
      const ratio = distance === 0 ? 0 : (strike - points[index - 1].strike) / distance;
      return index - 1 + ratio;
    }
  }
  return points.length - 1;
}

// 图表容器的内容尺寸（扣除内边距），用于让 SVG 与容器按 1:1 像素渲染。
function chartContentBox(target) {
  const rect = target.getBoundingClientRect();
  const style = getComputedStyle(target);
  return {
    width: Math.max(240, Math.round(rect.width - parseFloat(style.paddingLeft || 0) - parseFloat(style.paddingRight || 0))),
    height: Math.max(140, Math.round(rect.height - parseFloat(style.paddingTop || 0) - parseFloat(style.paddingBottom || 0))),
  };
}

// 柱端标记的文字宽度测量上下文（与 CSS 中 10px 粗体一致），用于贴边时避免文字被 SVG 视口裁剪。
let markerLabelContext = null;
function markerLabelWidth(text, weight = 700) {
  if (!markerLabelContext) markerLabelContext = document.createElement("canvas").getContext("2d");
  markerLabelContext.font = `${weight} 10px Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`;
  return markerLabelContext.measureText(text).width;
}

// Track the chart container, not just the SVG: the HTML spot-price badge sits
// above it and leaving the plot for that badge must also clear the readout.
// Mouse movement can exceed the display refresh rate, so process one point per frame.
function scheduleChartPointerMove(chartTarget, onMove, hideTooltip) {
  let frameId = null;
  let pending = null;
  const useAnimationFrame = typeof window.requestAnimationFrame === "function"
    && typeof window.cancelAnimationFrame === "function";
  const run = () => {
    frameId = null;
    const event = pending;
    pending = null;
    if (event) onMove(event);
  };
  const schedule = (event) => {
    pending = { clientX: event.clientX, clientY: event.clientY, buttons: event.buttons || 0, target: event.target };
    if (frameId !== null) return;
    frameId = useAnimationFrame ? window.requestAnimationFrame(run) : window.setTimeout(run, 16);
  };
  const cancel = () => {
    pending = null;
    if (frameId === null) return;
    if (useAnimationFrame) window.cancelAnimationFrame(frameId);
    else window.clearTimeout(frameId);
    frameId = null;
  };
  chartTarget.addEventListener("pointermove", schedule);
  const stop = () => { cancel(); hideTooltip(); };
  chartTarget.addEventListener("pointerleave", stop);
  chartTarget.addEventListener("pointercancel", stop);
  cancel.dispose = () => {
    cancel();
    chartTarget.removeEventListener("pointermove", schedule);
    chartTarget.removeEventListener("pointerleave", stop);
    chartTarget.removeEventListener("pointercancel", stop);
  };
  return cancel;
}

function chanStructureTooltipDetails(structure, fallbackRuleVersion = "未知") {
  if (!structure) return null;
  const displayPrice = (value) => value != null && value !== "" && Number.isFinite(Number(value)) ? formatMoney(Number(value)) : "未知";
  const startPrice = displayPrice(structure.startPrice);
  const endPrice = displayPrice(structure.endPrice);
  const start = structure.start || "未知";
  const end = structure.end || "未知";
  const formed = structure.formedAt ? `形成 ${structure.formedAt}` : "形成时间未知";
  const confirmation = structure.confirmedAt ? ` · 确认 ${structure.confirmedAt}` : " · 尚未确认";
  return {
    title: `${structure.type}${structure.level == null ? "" : ` · L${structure.level}`} · ${structure.confirmed ? "已确认" : "形成中"}`,
    body: `${start} ${startPrice} → ${end} ${endPrice} · ${formed}${confirmation} · ${structure.composition || "组成范围未知"} · 规则 ${structure.ruleVersion || fallbackRuleVersion || "未知"}`,
  };
}

// Canvas owns the candle geometry. This controller only updates the SVG hover
// layer and readout, so pointer movement never redraws the full chart.
function bindChanChartHover(target, initialModel) {
  const svg = target.querySelector("svg");
  const layer = target.querySelector(".chan-crosshair-layer");
  const tooltip = target.querySelector(".chan-chart-tooltip");
  if (!svg || !layer || !tooltip) return null;
  const vertical = layer.querySelector(".chart-crosshair");
  const horizontal = layer.querySelector(".chart-crosshair-h");
  const tagX = layer.querySelector(".chart-tag-x");
  const tagY = layer.querySelector(".chart-tag-y");
  let model = initialModel;
  let activeIndex = null;
  let lastPointer = null;
  let shownCandle = null;
  const title = document.createElement("div");
  title.className = "chart-tooltip-title";
  const subtitle = document.createElement("div");
  subtitle.className = "chart-tooltip-sub";
  const changeRow = document.createElement("div");
  changeRow.className = "chart-tooltip-row chan-ohlc-change-row";
  const changeLabel = document.createElement("span");
  changeLabel.textContent = "涨幅";
  const changeValue = document.createElement("strong");
  changeRow.append(changeLabel, changeValue);
  const structureDetail = document.createElement("section");
  structureDetail.className = "chan-structure-tooltip";
  structureDetail.hidden = true;
  const structureTitle = document.createElement("strong");
  const structureBody = document.createElement("span");
  structureDetail.append(structureTitle, structureBody);
  tooltip.replaceChildren(title, subtitle);
  const values = {};
  [["open", "开盘"], ["high", "最高"], ["low", "最低"], ["close", "收盘"]].forEach(([key, label]) => {
    const row = document.createElement("div");
    row.className = "chart-tooltip-row chan-ohlc-row";
    const name = document.createElement("span");
    name.textContent = label;
    values[key] = document.createElement("strong");
    row.append(name, values[key]);
    tooltip.append(row);
  });
  tooltip.append(changeRow);
  tooltip.append(structureDetail);
  const hide = () => {
    layer.setAttribute("visibility", "hidden");
    tooltip.hidden = true;
    activeIndex = null;
    lastPointer = null;
  };
  const localPoint = (clientX, clientY) => {
    const matrix = svg.getScreenCTM();
    if (!matrix) return null;
    const point = svg.createSVGPoint();
    point.x = clientX;
    point.y = clientY;
    return point.matrixTransform(matrix.inverse());
  };
  const setLine = (line, x1, y1, x2, y2) => {
    line.setAttribute("x1", x1); line.setAttribute("y1", y1);
    line.setAttribute("x2", x2); line.setAttribute("y2", y2);
  };
  const setTag = (tag, text, x, y, width) => {
    const rect = tag.querySelector("rect"), label = tag.querySelector("text");
    rect.setAttribute("x", x); rect.setAttribute("y", y); rect.setAttribute("width", width);
    label.setAttribute("x", x + width / 2); label.setAttribute("y", y + 12.5);
    label.setAttribute("text-anchor", "middle");
    label.textContent = text;
  };
  const show = (index, localY, localXOverride = null) => {
    const candle = model?.candles?.[index];
    if (!candle || !target.offsetWidth || !target.offsetHeight) { hide(); return; }
    activeIndex = index;
    const localX = Number.isFinite(localXOverride) ? localXOverride : candle.x;
    const plotHeight = model.plotBottom - model.plotTop;
    const priceRange = model.maximum - model.minimum;
    const cursorY = Math.max(model.plotTop, Math.min(model.plotBottom, localY));
    const price = model.maximum - (cursorY - model.plotTop) / plotHeight * priceRange;
    setLine(vertical, candle.x, model.plotTop, candle.x, model.plotBottom);
    // The Chan chart reserves its right side for scenario cards; keep the
    // historical crosshair inside the candle plot so it cannot strike text.
    setLine(horizontal, model.plotLeft, cursorY, model.historyRight ?? model.plotRight, cursorY);
    const displayDate = candle.displayDate || candle.date;
    const axisDate = displayDate.includes(" ") ? displayDate.slice(5) : displayDate;
    const dateWidth = Math.max(84, axisDate.length * 7 + 12);
    setTag(tagX, axisDate, Math.max(2, Math.min(model.width - dateWidth - 2, candle.x - dateWidth / 2)), model.height - 22, dateWidth);
    const priceLabel = formatMoney(price), priceWidth = Math.min(model.plotLeft - 4, Math.max(42, priceLabel.length * 6 + 10));
    // Preserve the current-price badge when the cursor reaches the same price.
    const overlapsSpot = model.spot && Math.abs(cursorY - model.spot.y) < model.spot.height / 2 + 10;
    const priceX = Math.max(2, model.plotLeft - priceWidth - 4);
    setTag(tagY, priceLabel, priceX, cursorY - 9, priceWidth);
    if (overlapsSpot) tagY.setAttribute("visibility", "hidden");
    else tagY.removeAttribute("visibility");
    if (shownCandle !== candle) {
      title.textContent = displayDate;
      subtitle.textContent = `${model.periodLabel} · 已收盘 K 线`;
      const directionClass = candle.prices.close > candle.prices.open ? "chan-ohlc-up"
        : candle.prices.close < candle.prices.open ? "chan-ohlc-down" : "chan-ohlc-flat";
      Object.keys(values).forEach((key) => {
        values[key].textContent = formatMoney(candle.prices[key]);
        values[key].className = `chan-ohlc-value ${directionClass}`;
      });
      const change = candle.closeChangePercent;
      changeValue.textContent = Number.isFinite(change)
        ? `${change > 0 ? "+" : ""}${change.toFixed(2)}%` : "未知";
      changeValue.className = `chan-ohlc-value ${!Number.isFinite(change) ? "chan-ohlc-flat" : change > 0 ? "chan-ohlc-up" : change < 0 ? "chan-ohlc-down" : "chan-ohlc-flat"}`;
      shownCandle = candle;
    }
    const distanceToSegment = (item) => {
      const dx = item.x2 - item.x1, dy = item.y2 - item.y1;
      const lengthSquared = dx * dx + dy * dy;
      const ratio = lengthSquared ? Math.max(0, Math.min(1, ((localX - item.x1) * dx + (localY - item.y1) * dy) / lengthSquared)) : 0;
      return Math.hypot(localX - (item.x1 + ratio * dx), localY - (item.y1 + ratio * dy));
    };
    const structure = (model.structures || []).map((item) => {
      const inArea = item.area && localX >= Math.min(item.x1, item.x2) && localX <= Math.max(item.x1, item.x2)
        && localY >= Math.min(item.y1, item.y2) && localY <= Math.max(item.y1, item.y2);
      return { item, distance: item.area ? (inArea ? 5 : Infinity) : distanceToSegment(item) };
    })
      .filter(({ item, distance }) => distance <= (item.area ? 0 : 8))
      .sort((left, right) => left.distance - right.distance)[0]?.item;
    if (structure) {
      const detail = chanStructureTooltipDetails(structure, model.ruleVersion);
      structureTitle.textContent = detail.title;
      structureBody.textContent = detail.body;
      structureDetail.hidden = false;
    } else {
      structureDetail.hidden = true;
      structureTitle.textContent = ""; structureBody.textContent = "";
    }
    layer.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    const matrix = svg.getScreenCTM();
    if (!matrix) { hide(); return; }
    const point = svg.createSVGPoint();
    point.x = candle.x; point.y = cursorY;
    const client = point.matrixTransform(matrix);
    const bounds = target.getBoundingClientRect();
    const tooltipWidth = tooltip.offsetWidth;
    tooltip.style.left = `${Math.max(4, Math.min(bounds.width - tooltipWidth - 4, client.x - bounds.left - tooltipWidth / 2))}px`;
    tooltip.style.top = "auto";
    tooltip.style.bottom = `${bounds.height + 4}px`;
  };
  const onMove = (event) => {
    if (event.target?.closest?.(".chan-spot-mobile-badge")) { hide(); return; }
    if (!model?.candles?.length || (event.buttons & 2)) { hide(); return; }
    const point = localPoint(event.clientX, event.clientY);
    if (!point || point.x < model.plotLeft || point.x > (model.historyRight ?? model.plotRight) || point.y < model.plotTop || point.y > model.plotBottom) { hide(); return; }
    lastPointer = event;
    const slot = ((model.historyRight ?? model.plotRight) - model.plotLeft) / model.candles.length;
    const index = Math.max(0, Math.min(model.candles.length - 1, Math.floor((point.x - model.plotLeft) / slot)));
    show(index, point.y, point.x);
  };
  const cancelPointerMove = scheduleChartPointerMove(target, onMove, hide);
  const showClose = (index) => {
    const close = model?.candles?.[index]?.prices.close;
    if (close == null) return;
    lastPointer = null;
    show(index, model.plotTop + (model.maximum - close) / (model.maximum - model.minimum) * (model.plotBottom - model.plotTop));
  };
  const onBlur = () => { cancelPointerMove(); hide(); };
  const onKey = (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End", "Escape"].includes(event.key)) return;
    event.preventDefault();
    cancelPointerMove();
    if (event.key === "Escape") { hide(); return; }
    const last = model.candles.length - 1;
    const current = activeIndex ?? last;
    const next = event.key === "Home" ? 0 : event.key === "End" ? last : current + (event.key === "ArrowLeft" ? -1 : 1);
    showClose(Math.max(0, Math.min(last, next)));
  };
  const onDown = (event) => {
    cancelPointerMove();
    if (event.button === 2) { hide(); return; }
    onMove(event);
  };
  svg.addEventListener("blur", onBlur);
  svg.addEventListener("keydown", onKey);
  svg.addEventListener("pointerdown", onDown);
  return {
    svg,
    update(next) {
      const activeDate = activeIndex == null ? null : model?.candles?.[activeIndex]?.date;
      model = next;
      if (lastPointer) onMove(lastPointer);
      else if (activeDate) {
        const nextIndex = model?.candles?.findIndex((candle) => candle.date === activeDate) ?? -1;
        if (nextIndex >= 0) showClose(nextIndex);
        else hide();
      }
    },
    destroy() {
      cancelPointerMove.dispose();
      hide();
      svg.removeEventListener("blur", onBlur);
      svg.removeEventListener("keydown", onKey);
      svg.removeEventListener("pointerdown", onDown);
    },
  };
}

const chanHoverControllers = new WeakMap();
const chanHoverBindRetries = new WeakMap();
function syncChanChartHover(element, binding, attempt = 0) {
  let controller = chanHoverControllers.get(element);
  if (controller && controller.svg !== element.querySelector("svg")) {
    controller.destroy();
    chanHoverControllers.delete(element);
    controller = null;
  }
  if (controller) controller.update(binding.value);
  else {
    controller = bindChanChartHover(element, binding.value);
    if (controller) chanHoverControllers.set(element, controller);
    else if (attempt < 4 && !chanHoverBindRetries.has(element)) {
      const retry = window.requestAnimationFrame(() => {
        chanHoverBindRetries.delete(element);
        if (element.isConnected) syncChanChartHover(element, binding, attempt + 1);
      });
      chanHoverBindRetries.set(element, retry);
    }
  }
}
const chanChartHover = {
  inserted: syncChanChartHover,
  componentUpdated: syncChanChartHover,
  unbind(element) {
    const retry = chanHoverBindRetries.get(element);
    if (retry != null) window.cancelAnimationFrame(retry);
    chanHoverBindRetries.delete(element);
    chanHoverControllers.get(element)?.destroy();
    chanHoverControllers.delete(element);
  },
};

// The Chan history layer can contain hundreds or thousands of individual
// chart marks. Paint those marks into one canvas instead of making each a DOM
// node; keep only the small, interactive scenario overlay in SVG.
function chanThemeColor(target, token, fallback) {
  const value = getComputedStyle(target).getPropertyValue(token).trim();
  return value || fallback;
}

function chanPaleCandleColor(color, amount = 0.54) {
  const hex = /^#([0-9a-f]{6})$/i.exec(color || "");
  if (!hex) return color;
  const value = Number.parseInt(hex[1], 16);
  const channel = (shift) => Math.round(((value >> shift) & 0xff) * (1 - amount) + 255 * amount);
  return "rgb(" + channel(16) + ", " + channel(8) + ", " + channel(0) + ")";
}

function drawChanCanvas(target, binding) {
  // The chart model has already filtered segments by the global switch and
  // per-level overrides; testing the global switch again would hide a level
  // that was explicitly enabled on its own.
  const chartLayers = binding?.chart?.layers;
  const segmentPaintEnabled = chartLayers && Object.prototype.hasOwnProperty.call(chartLayers, "segments")
    ? chartLayers.segments === true : binding?.layers?.segments === true;
  const model = binding?.chart ? { ...binding.chart, layers: { ...(chartLayers || {}), ...(binding.layers || {}), segments: segmentPaintEnabled } } : binding;
  const canvas = target.querySelector("canvas.chan-chart-canvas");
  const svg = target.querySelector("svg");
  if (!canvas || !svg || !model) return;
  const bounds = svg.getBoundingClientRect();
  const width = bounds.width, height = bounds.height;
  if (!width || !height || !model.width || !model.height) return;
  const ratio = Math.max(1, Math.min(3, window.devicePixelRatio || 1));
  const pixelWidth = Math.round(width * ratio), pixelHeight = Math.round(height * ratio);
  if (canvas.width !== pixelWidth || canvas.height !== pixelHeight) {
    canvas.width = pixelWidth; canvas.height = pixelHeight;
  }
  const context = canvas.getContext("2d");
  if (!context) return;
  canvas.style.width = `${width}px`; canvas.style.height = `${height}px`;
  context.setTransform(pixelWidth / model.width, 0, 0, pixelHeight / model.height, 0, 0);
  context.clearRect(0, 0, model.width, model.height);
  const colors = {
    grid: chanThemeColor(target, "--grid", "#344654"), axis: chanThemeColor(target, "--axis", "#81909c"),
    text: chanThemeColor(target, "--muted", "#81909c"), textPrimary: chanThemeColor(target, "--text", "#e8eef3"), panel: chanThemeColor(target, "--panel", "#101923"),
    up: chanThemeColor(target, "--up", "#19bd83"), down: chanThemeColor(target, "--down", "#f04d68"),
    blue: chanThemeColor(target, "--blue", "#69b7ff"), spot: chanThemeColor(target, "--muted", "#81909c"),
    amber: chanThemeColor(target, "--amber", "#e7b45c"), net: chanThemeColor(target, "--net", "#5478ff"), gex: chanThemeColor(target, "--gex", "#ff7c45"),
  };
  const levelColors = [colors.blue, colors.amber, colors.net, colors.gex, colors.up, colors.down, colors.text, colors.textPrimary];
  const levelColor = (level) => levelColors[Math.max(0, Number(level) || 0) % levelColors.length];
  const levelDash = (level) => [[], [7, 3], [3, 2], [9, 3, 2, 3], [1, 2], [6, 2, 1, 2], [10, 4], [4, 4]][Math.max(0, Number(level) || 0) % 8];
  const line = (x1, y1, x2, y2, color, dash = [], lineWidth = 1) => {
    context.beginPath(); context.setLineDash(dash); context.lineWidth = lineWidth; context.strokeStyle = color;
    context.moveTo(x1, y1); context.lineTo(x2, y2); context.stroke(); context.setLineDash([]);
  };
  const label = (text, x, y, options = {}) => {
    context.font = options.font || `${model.mobileChart ? 14 : 11}px Inter, ui-sans-serif, system-ui, sans-serif`;
    context.textAlign = options.align || "left"; context.textBaseline = options.baseline || "alphabetic";
    context.fillStyle = options.color || colors.text; context.fillText(text, x, y);
  };
  // Plot grid and price labels.
  for (const row of model.grid || []) {
    line(model.plotLeft, row.y, model.plotRight, row.y, colors.grid, [2, 4], 1);
    if (row.labelVisible) label(row.label, model.plotLeft - 8, row.y + 4, { align: "right" });
  }
  // Center zones sit behind candles. Legacy pen centers and recursive level
  // centers have independent switches so the hierarchy can be inspected.
  if (model.layers?.penCenters !== false) for (const zone of model.centers || []) {
    context.fillStyle = colors.blue; context.globalAlpha = zone.latest ? 0.14 : 0.08;
    context.fillRect(zone.x, zone.y, zone.width, zone.height);
    context.strokeStyle = colors.blue; context.globalAlpha = zone.latest ? 0.6 : 0.3;
    context.lineWidth = zone.latest ? 1.25 : 1;
    context.strokeRect(zone.x, zone.y, zone.width, zone.height);
    context.globalAlpha = 1;
  }
  for (const zone of model.recursiveCenters || []) {
    if (model.layers?.recursiveCenters?.[zone.level] !== true) continue;
    const color = levelColor(zone.level);
    context.fillStyle = color; context.globalAlpha = zone.forming ? 0.045 : 0.085;
    context.fillRect(zone.x, zone.y, zone.width, zone.height);
    context.strokeStyle = color; context.globalAlpha = zone.forming ? 0.72 : 0.46;
    context.lineWidth = Math.min(2.6, 1 + (zone.extensionCount || 0) * 0.18);
    context.setLineDash(zone.forming ? [5, 3] : levelDash(zone.level)); context.strokeRect(zone.x, zone.y, zone.width, zone.height); context.setLineDash([]);
    if (zone.width > 22 && zone.height > 12) label("L" + zone.level, zone.x + 4, zone.y + 10, { font: "9px Inter, ui-sans-serif, system-ui, sans-serif", color });
    context.globalAlpha = 1;
  }
  // Each original candle keeps its normal-colored body. The Chan layer paints
  // the merged high-low range back onto every source slot it covers, with an
  // identically sized pale column behind that body.
  const chanCandles = new Map((model.layers?.chanCandles === false ? [] : model.chanCandles || [])
    .map((candle) => [candle.candleIndex, candle]));
  for (const candle of model.candles || []) {
    const color = candle.direction === "up" ? colors.up : colors.down;
    const chan = chanCandles.get(candle.index);
    if (chan) {
      const pale = chanPaleCandleColor(chan.direction === "up" ? colors.up : colors.down);
      // Keep each source candle's real high/low visible even when inclusion
      // merging moves the merged range inward. Include its body bounds too so
      // the pale column joins the unchanged body without leaving a white gap.
      const bodyTop = candle.body;
      const bodyBottom = candle.body + candle.bodyHeight;
      const wickTop = Math.min(chan.high, chan.low, candle.high, candle.low, bodyTop);
      const wickBottom = Math.max(chan.high, chan.low, candle.high, candle.low, bodyBottom);
      context.fillStyle = pale;
      context.fillRect(candle.x - candle.bodyWidth / 2, wickTop, candle.bodyWidth, Math.max(1, wickBottom - wickTop));
    } else {
      line(candle.x, candle.high, candle.x, candle.low, color, [], 1);
    }
    context.fillStyle = color; context.strokeStyle = color; context.lineWidth = 1;
    context.fillRect(candle.x - candle.bodyWidth / 2, candle.body, candle.bodyWidth, candle.bodyHeight);
  }
  // Recursive segments are drawn as their own layer. Confirmed segments are
  // solid; the current forming endpoint is dashed. Hue identifies the level.
  if (model.layers?.segments === true) for (const segment of model.segments || []) {
    line(segment.x1, segment.y1, segment.x2, segment.y2, levelColor(segment.level), segment.confirmed ? levelDash(segment.level) : [6, 4], segment.level === 0 ? 2.6 : 2.1);
  }
  // Chan pens and endpoints can be hidden independently of segments.
  if (model.layers?.pens !== false) for (const pen of model.strokes || []) line(pen.x1, pen.y1, pen.x2, pen.y2, colors.blue, pen.confirmed ? [] : [6, 4], 2.2);
  if (model.layers?.pens !== false) for (const point of model.points || []) {
    context.beginPath(); context.arc(point.x, point.y, 2.6, 0, Math.PI * 2);
    context.fillStyle = point.confirmed ? colors.blue : colors.panel; context.fill();
    context.strokeStyle = colors.blue; context.lineWidth = point.confirmed ? 1 : 1.5; context.stroke();
    context.font = model.mobileChart ? "550 12px Inter, ui-sans-serif, system-ui, sans-serif" : "9px Inter, ui-sans-serif, system-ui, sans-serif";
    context.textAlign = "center"; context.textBaseline = "alphabetic";
    context.lineWidth = 3; context.strokeStyle = colors.panel; context.lineJoin = "round";
    context.strokeText(point.label, point.labelX, point.labelY); context.fillStyle = colors.textPrimary; context.fillText(point.label, point.labelX, point.labelY);
  }
  if (model.layers?.signals !== false) for (const marker of model.markers || []) {
    context.beginPath(); context.arc(marker.x, marker.y, 5, 0, Math.PI * 2);
    context.fillStyle = colors.panel; context.strokeStyle = colors.blue; context.lineWidth = 1.5; context.setLineDash(marker.watch ? [2, 2] : []); context.fill(); context.stroke(); context.setLineDash([]);
    context.font = "9px Inter, ui-sans-serif, system-ui, sans-serif"; context.textAlign = "center";
    context.lineWidth = 3; context.strokeStyle = colors.panel; context.strokeText(marker.label, marker.labelX, marker.labelY);
    context.fillStyle = colors.blue; context.fillText(marker.label, marker.labelX, marker.labelY);
  }
  // Chan structural reference levels: recent swing highs/lows and the latest center boundaries.
  for (const level of model.keyLevels || []) {
    const color = level.tone === "support" ? colors.up : level.tone === "resistance" ? colors.down : colors.blue;
    line(level.x1, level.y, level.x2, level.y, color, [3, 4], 1);
  }
  if (model.layers?.signals === true) for (const signal of model.signals || []) {
    const color = signal.side === "buy" ? colors.up : colors.down;
    context.beginPath();
    context.moveTo(signal.x, signal.y + (signal.side === "buy" ? 6 : -6));
    context.lineTo(signal.x - 5, signal.y + (signal.side === "buy" ? -2 : 2));
    context.lineTo(signal.x + 5, signal.y + (signal.side === "buy" ? -2 : 2));
    context.closePath(); context.fillStyle = color; context.globalAlpha = signal.status === "confirmed" ? 0.95 : 0.64; context.fill(); context.globalAlpha = 1;
    const status = signal.status === "confirmed" ? "✓" : "?";
    label(signal.label + status, signal.x, signal.y + (signal.side === "buy" ? 16 : -10), { align: "center", font: "9px Inter, ui-sans-serif, system-ui, sans-serif", color });
  }
  if (model.spot) line(model.plotLeft, model.spot.y, model.historyRight, model.spot.y, colors.spot, [3, 4], 1);
  // Bottom axis and the three visible date anchors.
  line(model.plotLeft, model.plotBottom, model.plotRight, model.plotBottom, colors.axis, [], 1);
  for (const date of model.dates || []) {
    const align = date.x < model.width * .1 ? "left" : date.x > model.width * .9 ? "right" : "center";
    label(date.label, date.x, date.y, { align });
  }
}

const chanCanvasControllers = new WeakMap();
function syncChanCanvas(element, binding) {
  let controller = chanCanvasControllers.get(element);
  if (!controller) {
    const render = () => drawChanCanvas(element, controller?.model);
    controller = { model: binding.value, render, observer: null, themeObserver: null };
    if (typeof ResizeObserver !== "undefined") { controller.observer = new ResizeObserver(render); controller.observer.observe(element); }
    if (typeof MutationObserver !== "undefined") {
      controller.themeObserver = new MutationObserver(render);
      controller.themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    }
    window.addEventListener("resize", render);
    chanCanvasControllers.set(element, controller);
  }
  controller.model = binding.value;
  window.requestAnimationFrame(controller.render);
}
const chanChartCanvas = {
  inserted: syncChanCanvas,
  componentUpdated: syncChanCanvas,
  unbind(element) {
    const controller = chanCanvasControllers.get(element);
    if (!controller) return;
    controller.observer?.disconnect(); controller.themeObserver?.disconnect(); window.removeEventListener("resize", controller.render);
    chanCanvasControllers.delete(element);
  },
};

// Drag to pan the visible candle range. Modified wheel and two-finger pinch
// zoom around the pointer midpoint so the price action under it stays anchored.
const chanInteractionControllers = new WeakMap();
const chanInteractionBindRetries = new WeakMap();
function bindChanChartInteraction(element, binding) {
  const svg = element.querySelector("svg");
  if (!svg) return null;
  const state = { model: binding.value?.chart, onPan: binding.value?.onPan, onZoom: binding.value?.onZoom, pointers: new Map(), drag: null, pinch: null, lastPinchBars: null, suppressClick: false, clickTimer: null };
  const localPoint = (event) => {
    const matrix = svg.getScreenCTM();
    if (!matrix) return null;
    const point = svg.createSVGPoint(); point.x = event.clientX; point.y = event.clientY;
    return point.matrixTransform(matrix.inverse());
  };
  const plotRatio = (clientX) => {
    const point = localPoint({ clientX, clientY: 0 });
    const model = state.model;
    if (!point || !model) return 0.5;
    return Math.max(0, Math.min(1, (point.x - model.plotLeft) / Math.max(1, model.historyRight - model.plotLeft)));
  };
  const anchorIndex = (model, ratio) => model.windowStart + ratio * Math.max(0, model.visibleBars - 1);
  const touchPointers = () => [...state.pointers.values()].filter((pointer) => pointer.pointerType === "touch");
  const suppressNextClick = () => {
    state.suppressClick = true; window.clearTimeout(state.clickTimer);
    state.clickTimer = window.setTimeout(() => { state.suppressClick = false; }, 500);
  };
  const startPinch = () => {
    const points = touchPointers();
    if (points.length < 2 || !state.model?.candles?.length) return;
    state.drag = null;
    const first = localPoint(points[0]), second = localPoint(points[1]);
    if (!first || !second) return;
    const distance = Math.hypot(second.x - first.x, second.y - first.y);
    const midpoint = (points[0].clientX + points[1].clientX) / 2;
    const ratio = plotRatio(midpoint);
    state.pinch = { distance: Math.max(1, distance), visibleBars: state.model.visibleBars, anchorRatio: ratio, anchorIndex: anchorIndex(state.model, ratio) };
    state.lastPinchBars = state.model.visibleBars;
    element.classList.add("chan-chart-dragging");
  };
  const onPointerDown = (event) => {
    if (event.pointerType === "mouse" && event.button !== 0) return;
    if (event.pointerType !== "touch" && event.pointerType !== "mouse" && event.pointerType !== "pen") return;
    // SVG controls and projection paths own their click/touch gestures. Taking
    // pointer capture here retargets their click to the chart container.
    if (event.target.closest?.(".chan-scenario-toggle-svg, .chan-projection-up, .chan-projection-down, .chan-projection-range")) return;
    const point = localPoint(event);
    const model = state.model;
    if (!point || !model || point.x < model.plotLeft || point.x > model.historyRight || point.y < model.plotTop || point.y > model.plotBottom) return;
    state.pointers.set(event.pointerId, { clientX: event.clientX, clientY: event.clientY, pointerType: event.pointerType });
    try { element.setPointerCapture(event.pointerId); } catch (_) { /* Capture is not available in older browsers. */ }
    if (event.pointerType === "touch" && touchPointers().length >= 2) {
      startPinch();
      return;
    }
    state.drag = { pointerId: event.pointerId, clientX: point.x, clientY: point.y, startX: point.x, startY: point.y, moved: false, fractionalBars: 0, emittedBars: 0 };
  };
  const onPointerMove = (event) => {
    if (!state.pointers.has(event.pointerId)) return;
    state.pointers.set(event.pointerId, { clientX: event.clientX, clientY: event.clientY, pointerType: event.pointerType });
    const model = state.model;
    const points = touchPointers();
    if (state.pinch && points.length >= 2 && model?.candles?.length) {
      const first = localPoint(points[0]), second = localPoint(points[1]);
      if (!first || !second) return;
      const distance = Math.max(1, Math.hypot(second.x - first.x, second.y - first.y));
      const scale = state.pinch.distance / distance;
      const nextVisibleBars = Math.max(Math.min(12, model.totalBars), Math.min(model.totalBars, Math.round(state.pinch.visibleBars * scale)));
      if (nextVisibleBars === state.lastPinchBars) return;
      state.lastPinchBars = nextVisibleBars;
      suppressNextClick();
      state.onZoom?.(scale, state.pinch.anchorIndex, state.pinch.anchorRatio, state.pinch.visibleBars);
      return;
    }
    if (!state.drag || state.drag.pointerId !== event.pointerId || !model?.candles?.length) return;
    const point = localPoint(event);
    if (!point) return;
    const dx = point.x - state.drag.clientX, dy = point.y - state.drag.clientY;
    const totalX = point.x - state.drag.startX, totalY = point.y - state.drag.startY;
    if (!state.drag.moved && Math.abs(totalX) > 5 && Math.abs(totalX) > Math.abs(totalY)) {
      state.drag.moved = true;
      element.classList.add("chan-chart-dragging");
    }
    const slot = (model.historyRight - model.plotLeft) / model.candles.length;
    if (Math.abs(dx) > 0.25 && state.drag.moved) {
      state.drag.fractionalBars += dx / Math.max(1, slot);
      const wholeBars = Math.trunc(state.drag.fractionalBars);
      const deltaBars = wholeBars - state.drag.emittedBars;
      if (deltaBars) { state.onPan?.(deltaBars); state.drag.emittedBars = wholeBars; }
      state.drag.clientX = point.x; state.drag.clientY = point.y;
    }
  };
  const onPointerUp = (event) => {
    const pointer = state.pointers.get(event.pointerId);
    state.pointers.delete(event.pointerId);
    if (state.pinch && touchPointers().length < 2) suppressNextClick();
    if (state.drag?.pointerId === event.pointerId) {
      if (state.drag.moved) {
        suppressNextClick();
      }
      state.drag = null;
    }
    if (state.pinch && touchPointers().length < 2) {
      state.pinch = null;
      const remaining = touchPointers()[0];
      const remainingPointerId = remaining ? [...state.pointers.entries()].find(([, value]) => value === remaining)?.[0] : null;
      const remainingPoint = remaining ? localPoint(remaining) : null;
      state.drag = remaining && remainingPoint ? { pointerId: remainingPointerId, clientX: remainingPoint.x, clientY: remainingPoint.y, startX: remainingPoint.x, startY: remainingPoint.y, moved: true, fractionalBars: 0, emittedBars: 0 } : null;
    }
    if (!state.pointers.size) element.classList.remove("chan-chart-dragging");
    if (pointer) { try { element.releasePointerCapture(event.pointerId); } catch (_) { /* Pointer capture may already be released. */ } }
  };
  const onClickCapture = (event) => {
    if (!state.suppressClick) return;
    window.clearTimeout(state.clickTimer);
    state.clickTimer = window.setTimeout(() => { state.suppressClick = false; }, 500);
    event.preventDefault(); event.stopPropagation();
  };
  const onWheel = (event) => {
    if (!(event.ctrlKey || event.metaKey) || !state.model?.candles?.length) return;
    event.preventDefault();
    const ratio = plotRatio(event.clientX);
    state.onZoom?.(event.deltaY < 0 ? 0.82 : 1.22, null, ratio);
  };
  element.addEventListener("pointerdown", onPointerDown);
  element.addEventListener("pointermove", onPointerMove);
  element.addEventListener("pointerup", onPointerUp);
  element.addEventListener("pointercancel", onPointerUp);
  element.addEventListener("lostpointercapture", onPointerUp);
  element.addEventListener("click", onClickCapture, true);
  // Run before SVG overlay targets so a chart layer cannot swallow the
  // modified wheel gesture after switching between Chan and technical modes.
  element.addEventListener("wheel", onWheel, { passive: false, capture: true });
  return {
    update(value) { state.model = value?.chart; state.onPan = value?.onPan; state.onZoom = value?.onZoom; },
    destroy() {
      window.clearTimeout(state.clickTimer); element.classList.remove("chan-chart-dragging");
      element.removeEventListener("pointerdown", onPointerDown); element.removeEventListener("pointermove", onPointerMove);
      element.removeEventListener("pointerup", onPointerUp); element.removeEventListener("pointercancel", onPointerUp);
      element.removeEventListener("lostpointercapture", onPointerUp); element.removeEventListener("click", onClickCapture, true);
      element.removeEventListener("wheel", onWheel, true);
    },
  };
}
function syncChanChartInteraction(element, binding, attempt = 0) {
  let controller = chanInteractionControllers.get(element);
  if (controller && controller.svg !== element.querySelector("svg")) { controller.destroy(); chanInteractionControllers.delete(element); controller = null; }
  if (controller) controller.update(binding.value);
  else {
    const interaction = bindChanChartInteraction(element, binding);
    if (interaction) { controller = { ...interaction, svg: element.querySelector("svg") }; chanInteractionControllers.set(element, controller); }
    else if (attempt < 4 && !chanInteractionBindRetries.has(element)) {
      const retry = window.requestAnimationFrame(() => {
        chanInteractionBindRetries.delete(element);
        if (element.isConnected) syncChanChartInteraction(element, binding, attempt + 1);
      });
      chanInteractionBindRetries.set(element, retry);
    }
  }
}
const chanChartInteraction = {
  inserted: syncChanChartInteraction,
  componentUpdated: syncChanChartInteraction,
  unbind(element) {
    const retry = chanInteractionBindRetries.get(element);
    if (retry != null) window.cancelAnimationFrame(retry);
    chanInteractionBindRetries.delete(element);
    chanInteractionControllers.get(element)?.destroy(); chanInteractionControllers.delete(element);
  },
};

function renderSignedChart(targetId, points, positiveKey, negativeKey, unit, emptyMessage, options = {}) {
  const target = document.getElementById(targetId);
  if (!points.length || points.every((point) => !(Math.abs(point[positiveKey]) + Math.abs(point[negativeKey])))) { target.innerHTML = `<div class="chart-empty">${emptyMessage}</div>`; return; }
  // 按容器实际像素绘制：viewBox 与元素 1:1，手机窄屏不会再把柱子和坐标文字整体等比缩小。
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width); target.dataset.chartHeight = String(height);
  // 成交量/持仓量图按行情软件样式把数值列放在右侧，Gamma 图保留左侧数值列。
  const axisOnRight = options.axis === "right";
  const pad = axisOnRight ? { left: 16, right: 56, top: 14, bottom: 26 } : { left: 48, right: 12, top: 14, bottom: 26 }; const innerWidth = width - pad.left - pad.right; const innerHeight = height - pad.top - pad.bottom; const baseline = pad.top + innerHeight / 2;
  // 上下各留一条固定高度的「标签通道」：柱子最高只顶到通道内侧的标高线，柱端文字与方向角标住在通道里，不会再压到坐标刻度。
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16)); const scaleTop = pad.top + gutter; const scaleBottom = height - pad.bottom - gutter; const maxBarHeight = innerHeight / 2 - gutter;
  const maxValue = Math.max(1, ...points.map((point) => Math.max(Math.abs(point[positiveKey]), Math.abs(point[negativeKey])))); const slot = innerWidth / points.length; const barWidth = Math.max(3, Math.min(26, slot * 0.68));
  const yLabel = formatChartValue(maxValue, 2, unit);
  // 左侧数值列右边界：最左行权价的柱端文字压到数值时向右避让。
  const axisColumnWidth = 6 + Math.max(markerLabelWidth(yLabel, 400), markerLabelWidth(`-${yLabel}`, 400));
  const labelColumnRight = axisOnRight ? 0 : axisColumnWidth + 4;
  // 刻度文字按可用宽度决定数量：桌面宽屏约每 84px 一个，手机上自动减少避免重叠。
  const labelEvery = Math.max(1, Math.ceil(points.length / Math.max(2, Math.floor(innerWidth / 84))));
  const bars = points.map((point, index) => { const x = pad.left + index * slot + (slot - barWidth) / 2; const positive = Number(point[positiveKey]) || 0; const negative = Number(point[negativeKey]) || 0; const positiveHeight = Math.abs(positive) / maxValue * maxBarHeight; const negativeHeight = Math.abs(negative) / maxValue * maxBarHeight; const label = index % labelEvery === 0 ? `<text class="chart-label" x="${x + barWidth / 2}" y="${height - 8}" text-anchor="middle">${formatMoney(point.strike)}</text>` : ""; return `<rect class="chart-call" x="${x}" y="${baseline - positiveHeight}" width="${barWidth}" height="${positiveHeight}" rx="1"></rect><rect class="chart-put" x="${x}" y="${baseline}" width="${barWidth}" height="${negativeHeight}" rx="1"></rect>${label}`; }).join("");
  // 柱端标记：Gamma 图标注看涨墙/看跌墙，成交量与持仓量图标注最高看涨柱/最高看跌柱。
  const markers = (options.markers || []).filter((marker) => marker.point).map((marker) => {
    const index = points.indexOf(marker.point);
    if (index < 0) return "";
    const x = pad.left + index * slot + slot / 2;
    const positive = Number(marker.point[positiveKey]) || 0;
    const negative = Number(marker.point[negativeKey]) || 0;
    const barHeight = marker.position === "top"
      ? Math.abs(positive) / maxValue * maxBarHeight
      : Math.abs(negative) / maxValue * maxBarHeight;
    const tipY = marker.position === "top" ? baseline - barHeight : baseline + barHeight;
    const labelText = `${marker.label} ${formatMoney(marker.point.strike)}`;
    // 柱端文字始终贴在柱端外侧（看涨在上、看跌在下），并落在标签通道范围内，不会翻到柱身上。
    const labelY = Math.min(Math.max(marker.position === "top" ? tipY - 14 : tipY + 22, pad.top + 27), scaleBottom + 24);
    // 最左行权价的文字若压到左侧数值列则向右避让；最左/最右再按 SVG 视口收边，避免被裁剪。
    const halfLabel = markerLabelWidth(labelText) / 2 + 1;
    let labelX = Math.min(Math.max(x, halfLabel), width - halfLabel);
    const overScaleRow = [scaleTop, scaleBottom].some((line) => labelY - 11 < line + 7 && labelY + 3 > line - 7);
    if (overScaleRow && !axisOnRight && labelX - halfLabel < labelColumnRight) labelX = Math.min(labelColumnRight + halfLabel, width - halfLabel);
    if (overScaleRow && axisOnRight && labelX + halfLabel > width - axisColumnWidth) labelX = Math.max(width - axisColumnWidth - halfLabel, halfLabel);
    return `<text class="${marker.className}-label" x="${labelX}" y="${labelY}" text-anchor="middle">${labelText}</text>`;
  }).join("");
  const gammaFlip = options.gammaFlip;
  const gammaFlipMarker = gammaFlip ? (() => {
    const position = strikePosition(points, gammaFlip.strike);
    if (position == null) return "";
    const x = pad.left + position * slot + slot / 2;
    return `<line class="chart-gamma-flip" x1="${x}" x2="${x}" y1="${pad.top}" y2="${height - pad.bottom}"/>`;
  })() : "";
  // 右轴图表给出参考样式的 5 档刻度（±最大 / ±一半 / 0），Gamma 图保持 3 档。
  const halfTop = (scaleTop + baseline) / 2; const halfBottom = (scaleBottom + baseline) / 2;
  const halfLabel = formatChartValue(maxValue / 2, 2, unit);
  const axisLabels = axisOnRight
    ? `<text class="chart-label" x="${width - 6}" y="${scaleTop + 4}" text-anchor="end">${yLabel}</text><text class="chart-label" x="${width - 6}" y="${halfTop + 4}" text-anchor="end">${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${baseline + 4}" text-anchor="end">0</text><text class="chart-label" x="${width - 6}" y="${halfBottom + 4}" text-anchor="end">-${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${scaleBottom + 4}" text-anchor="end">-${yLabel}</text>`
    : `<text class="chart-label" x="4" y="${scaleTop + 4}">${yLabel}</text><text class="chart-label" x="4" y="${baseline + 4}">0</text><text class="chart-label" x="4" y="${scaleBottom + 4}">-${yLabel}</text>`;
  const gridLines = axisOnRight ? `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfTop}" y2="${halfTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfBottom}" y2="${halfBottom}"/>` : "";
  // 右轴图表不再画角落方向文字，方向改由图例说明，避免与右侧数值列抢位。
  const cornerLabels = axisOnRight ? "" : `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">看涨 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">看跌 ↓</text>`;
  const tagElements = options.crosshairTags ? `<line class="chart-crosshair-h" x1="${pad.left}" x2="${width - pad.right}" y1="0" y2="0" visibility="hidden"/><g class="chart-tag chart-tag-x" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g><g class="chart-tag chart-tag-y" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g>` : "";
  const svg = `<svg viewBox="0 0 ${width} ${height}" role="img" tabindex="0" aria-label="${target.getAttribute("aria-label") || "期权分布图"}">${gridLines}<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleTop}" y2="${scaleTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleBottom}" y2="${scaleBottom}"/><line class="chart-zero" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/>${axisLabels}${gammaFlipMarker}${bars}${markers}<line class="chart-crosshair" x1="0" x2="0" y1="${pad.top}" y2="${height - pad.bottom}" visibility="hidden"/>${tagElements}${cornerLabels}</svg><div class="chart-tooltip" hidden></div>`; target.innerHTML = svg;

  const chartSvg = target.querySelector("svg");
  const crosshair = target.querySelector(".chart-crosshair");
  const tooltip = target.querySelector(".chart-tooltip");
  const tooltipTitle = options.tooltipTitle || "行权价";
  // SVG 按等比缩放绘制，容器与绘图区之间可能有留白，坐标换算必须走屏幕矩阵，保证十字虚线与数据卡严格对齐。
  const viewBoxXToClient = (viewBoxX) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = viewBoxX;
      point.y = 0;
      return point.matrixTransform(matrix).x;
    }
    const rect = chartSvg.getBoundingClientRect();
    return rect.left + (viewBoxX / width) * rect.width;
  };
  const clientToViewBox = (clientX, clientY) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = clientX;
      point.y = clientY;
      return point.matrixTransform(matrix.inverse());
    }
    const rect = chartSvg.getBoundingClientRect();
    return { x: ((clientX - rect.left) / rect.width) * width, y: ((clientY - rect.top) / rect.height) * height };
  };
  const crosshairLine = target.querySelector(".chart-crosshair-h");
  const tagX = target.querySelector(".chart-tag-x");
  const tagY = target.querySelector(".chart-tag-y");
  // 十字光标取值标签（参考行情软件）：数值列一侧贴当前鼠标高度的取值，底部贴所在行权价。
  const updateTags = (index, clientY, centerX) => {
    if (!crosshairLine) return;
    const point = points[index];
    const hidden = clientY == null || !point;
    crosshairLine.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagX.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagY.setAttribute("visibility", hidden ? "hidden" : "visible");
    if (hidden) return;
    const localY = Math.min(Math.max(clientToViewBox(0, clientY).y, pad.top + 6), height - pad.bottom - 6);
    crosshairLine.setAttribute("y1", localY);
    crosshairLine.setAttribute("y2", localY);
    const value = (baseline - localY) / maxBarHeight * maxValue;
    const valueText = `${value < 0 ? "-" : ""}${formatChartValue(Math.abs(value), 2, unit)}`;
    const valueWidth = markerLabelWidth(valueText, 700) + 10;
    const valueLeft = axisOnRight ? width - valueWidth - 4 : 4;
    tagY.querySelector("rect").setAttribute("x", valueLeft);
    tagY.querySelector("rect").setAttribute("y", localY - 8);
    tagY.querySelector("rect").setAttribute("width", valueWidth);
    const valueNode = tagY.querySelector("text");
    valueNode.setAttribute("x", valueLeft + 5);
    valueNode.setAttribute("y", localY + 4);
    valueNode.textContent = valueText;
    const strikeText = formatMoney(point.strike);
    const strikeWidth = markerLabelWidth(strikeText, 700) + 10;
    const strikeLeft = Math.min(Math.max(centerX - strikeWidth / 2, 2), width - strikeWidth - 2);
    tagX.querySelector("rect").setAttribute("x", strikeLeft);
    tagX.querySelector("rect").setAttribute("y", height - 20);
    tagX.querySelector("rect").setAttribute("width", strikeWidth);
    const strikeNode = tagX.querySelector("text");
    strikeNode.setAttribute("x", strikeLeft + 5);
    strikeNode.setAttribute("y", height - 8);
    strikeNode.textContent = strikeText;
  };
  const showTooltip = (index, clientY) => {
    const point = points[index];
    if (!point) return;
    const positive = Number(point[positiveKey]) || 0;
    const negative = Number(point[negativeKey]) || 0;
    const isGamma = targetId === "gex-chart";
    const net = isGamma ? positive + negative : positive - negative;
    const valueLabel = isGamma ? " GEX" : (options.valueLabel || "");
    const rows = [
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot call"></i>看涨${valueLabel}</span><strong>${formatChartValue(Math.abs(positive), 2, unit)}</strong></div>`,
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot put"></i>看跌${valueLabel}</span><strong>${formatChartValue(Math.abs(negative), 2, unit)}</strong></div>`,
    ];
    if (isGamma) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>净 GEX</span><strong>${formatChartValue(net, 2, unit)}</strong></div>`);
      if (options.spot != null) rows.unshift(`<div class="chart-tooltip-sub">行情价 ${formatMoney(options.spot)}</div>`);
    } else if (options.valueLabel) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>总${options.valueLabel}</span><strong>${formatChartValue(Math.abs(positive) + Math.abs(negative), 2, unit)}</strong></div>`);
    }
    tooltip.innerHTML = `<div class="chart-tooltip-title">${tooltipTitle} ${formatMoney(point.strike)}</div>${rows.join("")}`;
    const x = pad.left + index * slot + slot / 2;
    crosshair.setAttribute("x1", x);
    crosshair.setAttribute("x2", x);
    crosshair.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    const targetRect = target.getBoundingClientRect();
    const tooltipWidth = tooltip.offsetWidth || 180;
    // 数据卡固定悬浮在图表容器上沿之外（不进入图表内部），横向跟随十字虚线，纵向不跟随鼠标。
    const lineClientX = viewBoxXToClient(x);
    const left = Math.max(8, Math.min(targetRect.width - tooltipWidth - 8, lineClientX - targetRect.left - tooltipWidth / 2));
    tooltip.style.left = `${left}px`;
    tooltip.style.top = "auto";
    tooltip.style.bottom = `${targetRect.height + 4}px`;
    updateTags(index, clientY, x);
  };
  const hideTooltip = () => {
    crosshair.setAttribute("visibility", "hidden");
    tooltip.hidden = true;
    if (crosshairLine) crosshairLine.setAttribute("visibility", "hidden");
    if (tagX) tagX.setAttribute("visibility", "hidden");
    if (tagY) tagY.setAttribute("visibility", "hidden");
  };
  const pointFromEvent = (event) => {
    const local = clientToViewBox(event.clientX, event.clientY);
    return Math.max(0, Math.min(points.length - 1, Math.floor((local.x - pad.left) / slot)));
  };
  const cancelPointerMove = scheduleChartPointerMove(chartSvg, (event) => {
    // 右键按住或拖动时不更新悬浮窗，避免浏览器菜单交互产生异常坐标。
    if (event.buttons & 2) {
      hideTooltip();
      return;
    }
    showTooltip(pointFromEvent(event), event.clientY);
  }, hideTooltip);
  chartSvg.addEventListener("pointerdown", (event) => {
    if (event.button !== 2) return;
    event.preventDefault();
    cancelPointerMove();
    hideTooltip();
  });
  chartSvg.addEventListener("contextmenu", (event) => {
    event.preventDefault();
    cancelPointerMove();
    hideTooltip();
  });
  chartSvg.addEventListener("focus", () => showTooltip(0, null));
  chartSvg.addEventListener("blur", hideTooltip);
}
function renderDistributionSummary(target, rows, spot, key, totalLabel) {
  if (!target) return;
  const summary = summarizeDistribution(rows, spot, key);
  const line = (label, bucket) => `<tr><th>${label}</th><td>${formatCount(bucket.all)}</td><td>${formatCount(bucket.itm)}</td><td>${formatCount(bucket.all - bucket.itm)}</td></tr>`;
  const total = { all: summary.call.all + summary.put.all, itm: summary.call.itm + summary.put.itm };
  target.innerHTML = `<table><thead><tr><th></th><th>全部</th><th>价内</th><th>价外</th></tr></thead><tbody>${line("看涨", summary.call)}${line("看跌", summary.put)}${line(totalLabel, total)}</tbody></table>`;
}
// 压力位向上（绿）、支撑位向下（红）；悬停显示代表价、距现价、触及概率与综合依据。
function renderLevelsChart(payload) {
  const target = document.getElementById("levels-chart");
  if (!target) return;
  const spot = Number(payload?.spot);
  const levels = [
    ...(payload?.resistance || []).map((item) => ({ ...item, side: "up" })),
    ...(payload?.support || []).map((item) => ({ ...item, side: "down" })),
  ]
    .map((item) => ({ price: Number(item.price), score: Math.max(0, Number(item.score) || 0), probability: item.probability, factors: item.factors || [], side: item.side }))
    .filter((item) => Number.isFinite(item.price) && item.price > 0);
  const basisBadge = document.getElementById("levels-basis");
  if (basisBadge) basisBadge.textContent = Number.isFinite(spot) && spot > 0 ? `基准 ${formatMoney(spot)}` : "基准 --";
  if (!levels.length) { target.innerHTML = '<div class="chart-empty">暂无压力位/支撑位数据</div>'; return; }
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width); target.dataset.chartHeight = String(height);
  const pad = { left: 48, right: 12, top: 14, bottom: 26 };
  const innerWidth = width - pad.left - pad.right;
  const innerHeight = height - pad.top - pad.bottom;
  const baseline = pad.top + innerHeight / 2;
  // 上下各留一条「标签通道」，柱高最高只顶到通道内侧，柱端不会压到坐标刻度。
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16));
  const maxBarHeight = innerHeight / 2 - gutter;
  const maxScore = Math.max(0.01, ...levels.map((item) => item.score));
  const ordered = [...levels].sort((a, b) => a.price - b.price);
  const prices = ordered.map((item) => item.price);
  const rawMin = prices[0];
  const rawMax = prices[prices.length - 1];
  const span = rawMax - rawMin || Math.max(1, rawMax * 0.02);
  // 横轴按价格线性排布（真实反映价位间距），首尾各留 6% 余量避免柱子贴边。
  const domainMin = rawMin - span * 0.06;
  const domainMax = rawMax + span * 0.06;
  const scale = innerWidth / (domainMax - domainMin);
  const xOf = (price) => pad.left + (price - domainMin) * scale;
  // 柱宽取相邻价位最小间距的 60%，价位密集时柱子也不会互相压盖。
  const gaps = prices.slice(1).map((value, index) => value - prices[index]).filter((value) => value > 0);
  const minGap = gaps.length ? Math.min(...gaps) : span;
  const barWidth = Math.max(3, Math.min(26, minGap * scale * 0.6));
  const yLabel = maxScore.toFixed(2);
  // 价位刻度按像素间距抽样：横轴按价格线性排布、间距不均，必须按实际像素判断而不是按条数取样。
  const minLabelGap = 46;
  let lastLabelX = -Infinity;
  const barNodes = ordered.map((item) => {
    const centerX = xOf(item.price);
    const barHeight = item.score / maxScore * maxBarHeight;
    const y = item.side === "up" ? baseline - barHeight : baseline;
    const className = item.side === "up" ? "chart-call" : "chart-put";
    const showLabel = centerX - lastLabelX >= minLabelGap;
    if (showLabel) lastLabelX = centerX;
    const label = showLabel ? `<text class="chart-label" x="${centerX}" y="${height - 8}" text-anchor="middle">${formatMoney(item.price)}</text>` : "";
    return `<rect class="${className}" x="${centerX - barWidth / 2}" y="${y}" width="${barWidth}" height="${barHeight}" rx="1"></rect>${label}`;
  }).join("");
  const gridLines = [baseline - maxBarHeight, baseline, baseline + maxBarHeight].map((line) => `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${line}" y2="${line}"/>`).join("");
  const axisLabels = `<text class="chart-label" x="4" y="${baseline - maxBarHeight + 4}">${yLabel}</text><text class="chart-label" x="4" y="${baseline + 4}">0</text><text class="chart-label" x="4" y="${baseline + maxBarHeight + 4}">-${yLabel}</text>`;
  const basisLine = Number.isFinite(spot) && spot > 0 && spot >= domainMin && spot <= domainMax
    ? `<line class="chart-level-basis" x1="${xOf(spot)}" x2="${xOf(spot)}" y1="${pad.top}" y2="${height - pad.bottom}"/><text class="chart-label" x="${Math.min(Math.max(xOf(spot), pad.left + 14), width - pad.right - 14)}" y="${pad.top + 10}" text-anchor="middle">基准</text>`
    : "";
  const cornerLabels = `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">压力 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">支撑 ↓</text>`;
  // 与成交量/持仓量图一致的十字虚线：竖向跟随鼠标所在价位、横向贴鼠标高度，两端各带取值标签。
  const crosshairTags = `<line class="chart-crosshair-h" x1="${pad.left}" x2="${width - pad.right}" y1="0" y2="0" visibility="hidden"/><g class="chart-tag chart-tag-x" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g><g class="chart-tag chart-tag-y" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g>`;
  target.innerHTML = `<svg viewBox="0 0 ${width} ${height}" role="img" tabindex="0" aria-label="压力位/支撑位柱状图">${gridLines}${axisLabels}${basisLine}${barNodes}${cornerLabels}<line class="chart-crosshair" x1="0" x2="0" y1="${pad.top}" y2="${height - pad.bottom}" visibility="hidden"/>${crosshairTags}</svg><div class="chart-tooltip" hidden></div>`;

  const chartSvg = target.querySelector("svg");
  const crosshair = target.querySelector(".chart-crosshair");
  const crosshairLine = target.querySelector(".chart-crosshair-h");
  const tagX = target.querySelector(".chart-tag-x");
  const tagY = target.querySelector(".chart-tag-y");
  const tooltip = target.querySelector(".chart-tooltip");
  // SVG 与绘图区之间可能有留白，坐标换算走屏幕矩阵，保证十字虚线与数据卡严格对齐。
  const clientToViewBox = (clientX, clientY) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = clientX; point.y = clientY;
      return point.matrixTransform(matrix.inverse());
    }
    const rect = chartSvg.getBoundingClientRect();
    return { x: ((clientX - rect.left) / rect.width) * width, y: ((clientY - rect.top) / rect.height) * height };
  };
  const viewBoxXToClient = (viewBoxX) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = viewBoxX; point.y = 0;
      return point.matrixTransform(matrix).x;
    }
    const rect = chartSvg.getBoundingClientRect();
    return rect.left + (viewBoxX / width) * rect.width;
  };
  const nearestLevel = (clientX) => {
    const localX = clientToViewBox(clientX, 0).x;
    return ordered.reduce((best, item) => (best == null || Math.abs(xOf(item.price) - localX) < Math.abs(xOf(best.price) - localX) ? item : best), null);
  };
  // 十字光标取值标签：左侧贴当前鼠标高度的强度值，底部贴所在价位。
  const updateTags = (item, clientY) => {
    const hidden = clientY == null || !item;
    crosshairLine.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagX.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagY.setAttribute("visibility", hidden ? "hidden" : "visible");
    if (hidden) return;
    const localY = Math.min(Math.max(clientToViewBox(0, clientY).y, pad.top + 6), height - pad.bottom - 6);
    crosshairLine.setAttribute("y1", localY);
    crosshairLine.setAttribute("y2", localY);
    const value = (baseline - localY) / maxBarHeight * maxScore;
    const valueText = `${value < 0 ? "-" : ""}${Math.abs(value).toFixed(2)}`;
    const valueWidth = markerLabelWidth(valueText, 700) + 10;
    tagY.querySelector("rect").setAttribute("x", 4);
    tagY.querySelector("rect").setAttribute("y", localY - 8);
    tagY.querySelector("rect").setAttribute("width", valueWidth);
    const valueNode = tagY.querySelector("text");
    valueNode.setAttribute("x", 9);
    valueNode.setAttribute("y", localY + 4);
    valueNode.textContent = valueText;
    const priceText = formatMoney(item.price);
    const priceWidth = markerLabelWidth(priceText, 700) + 10;
    const priceLeft = Math.min(Math.max(xOf(item.price) - priceWidth / 2, 2), width - priceWidth - 2);
    tagX.querySelector("rect").setAttribute("x", priceLeft);
    tagX.querySelector("rect").setAttribute("y", height - 20);
    tagX.querySelector("rect").setAttribute("width", priceWidth);
    const priceNode = tagX.querySelector("text");
    priceNode.setAttribute("x", priceLeft + 5);
    priceNode.setAttribute("y", height - 8);
    priceNode.textContent = priceText;
  };
  const showTooltip = (item, clientY) => {
    if (!item) return;
    const gap = Number.isFinite(spot) && spot > 0 ? (item.price / spot - 1) * 100 : null;
    const gapText = gap == null ? "--" : `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    const sideLabel = item.side === "up" ? "压力位" : "支撑位";
    const rows = [
      `<div class="chart-tooltip-row"><span>距现价</span><strong>${gapText}</strong></div>`,
      `<div class="chart-tooltip-row"><span>到达概率</span><strong>${formatProbability(item.probability)}</strong></div>`,
      `<div class="chart-tooltip-row"><span>综合强度</span><strong>${item.score.toFixed(2)}</strong></div>`,
    ];
    const factors = (item.factors || []).join(" · ");
    tooltip.innerHTML = `<div class="chart-tooltip-title"><i class="tooltip-dot ${item.side === "up" ? "call" : "put"}"></i>${sideLabel} ${formatMoney(item.price)}</div>${rows.join("")}${factors ? `<div class="chart-tooltip-sub">${factors}</div>` : ""}`;
    crosshair.setAttribute("x1", xOf(item.price));
    crosshair.setAttribute("x2", xOf(item.price));
    crosshair.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    const targetRect = target.getBoundingClientRect();
    const tooltipWidth = tooltip.offsetWidth || 180;
    const lineClientX = viewBoxXToClient(xOf(item.price));
    tooltip.style.left = `${Math.max(8, Math.min(targetRect.width - tooltipWidth - 8, lineClientX - targetRect.left - tooltipWidth / 2))}px`;
    tooltip.style.top = "auto";
    tooltip.style.bottom = `${targetRect.height + 4}px`;
    updateTags(item, clientY);
  };
  const hideTooltip = () => { crosshair.setAttribute("visibility", "hidden"); tooltip.hidden = true; crosshairLine.setAttribute("visibility", "hidden"); tagX.setAttribute("visibility", "hidden"); tagY.setAttribute("visibility", "hidden"); };
  const cancelPointerMove = scheduleChartPointerMove(chartSvg, (event) => {
    // 右键按住或拖动时不更新悬浮窗，避免浏览器菜单交互产生异常坐标。
    if (event.buttons & 2) {
      hideTooltip();
      return;
    }
    showTooltip(nearestLevel(event.clientX), event.clientY);
  }, hideTooltip);
  chartSvg.addEventListener("pointerdown", (event) => {
    if (event.button !== 2) return;
    event.preventDefault();
    cancelPointerMove();
    hideTooltip();
  });
  chartSvg.addEventListener("contextmenu", (event) => {
    event.preventDefault();
    cancelPointerMove();
    hideTooltip();
  });
  chartSvg.addEventListener("focus", () => showTooltip(ordered[0], null));
  chartSvg.addEventListener("blur", hideTooltip);
}

// 数据还没回来时先铺坐标、零线和 0 刻度，避免四张图各留一大块空白。
function renderChartSkeleton(targetId, options = {}) {
  const target = document.getElementById(targetId);
  if (!target) return;
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width);
  target.dataset.chartHeight = String(height);
  const axisOnRight = options.axis === "right";
  const pad = axisOnRight ? { left: 16, right: 56, top: 14, bottom: 26 } : { left: 48, right: 12, top: 14, bottom: 26 };
  const innerWidth = width - pad.left - pad.right;
  const innerHeight = height - pad.top - pad.bottom;
  const baseline = pad.top + innerHeight / 2;
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16));
  const scaleTop = pad.top + gutter;
  const scaleBottom = height - pad.bottom - gutter;
  const halfTop = (scaleTop + baseline) / 2;
  const halfBottom = (scaleBottom + baseline) / 2;
  const zero = options.zeroLabel || "0";
  const axisLabels = axisOnRight
    ? `<text class="chart-label" x="${width - 6}" y="${scaleTop + 4}" text-anchor="end">${zero}</text><text class="chart-label" x="${width - 6}" y="${halfTop + 4}" text-anchor="end">${zero}</text><text class="chart-label" x="${width - 6}" y="${baseline + 4}" text-anchor="end">${zero}</text><text class="chart-label" x="${width - 6}" y="${halfBottom + 4}" text-anchor="end">${zero}</text><text class="chart-label" x="${width - 6}" y="${scaleBottom + 4}" text-anchor="end">${zero}</text>`
    : `<text class="chart-label" x="4" y="${scaleTop + 4}">${zero}</text><text class="chart-label" x="4" y="${baseline + 4}">${zero}</text><text class="chart-label" x="4" y="${scaleBottom + 4}">${zero}</text>`;
  const gridLines = axisOnRight
    ? `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfTop}" y2="${halfTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfBottom}" y2="${halfBottom}"/>`
    : "";
  const strikeCount = Math.max(2, Math.floor(innerWidth / 84));
  const strikes = Array.from({ length: strikeCount }, (_, index) => {
    const x = pad.left + (index + 0.5) * (innerWidth / strikeCount);
    return `<text class="chart-label" x="${x}" y="${height - 8}" text-anchor="middle">0.00</text>`;
  }).join("");
  const cornerLabels = options.corners === "levels"
    ? `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">压力 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">支撑 ↓</text>`
    : (axisOnRight ? "" : `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">看涨 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">看跌 ↓</text>`);
  const label = target.getAttribute("aria-label") || "图表";
  target.innerHTML = `<svg data-skeleton="1" viewBox="0 0 ${width} ${height}" role="img" aria-label="${label}">${gridLines}<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleTop}" y2="${scaleTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleBottom}" y2="${scaleBottom}"/><line class="chart-zero" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/>${axisLabels}${strikes}${cornerLabels}</svg>`;
}

function showEmpty(targetId, message) {
  const target = document.getElementById(targetId);
  if (target) target.innerHTML = `<div class="chart-empty">${message}</div>`;
}

function clearSummary(targetId) {
  const target = document.getElementById(targetId);
  if (target) target.textContent = "";
}

/* 图表模块只暴露绘制入口，页面状态不进入这里。 */
window.OptionScopeCharts = {
  chanChartCanvas: chanChartCanvas,
  renderChanCanvas: drawChanCanvas,
  chanChartInteraction: chanChartInteraction,
  chanChartHover: chanChartHover,
  chanStructureTooltipDetails: chanStructureTooltipDetails,
  renderSignedChart: renderSignedChart,
  renderLevelsChart: renderLevelsChart,
  renderDistributionSummary: renderDistributionSummary,
  renderChartSkeleton: renderChartSkeleton,
  chartContentBox: chartContentBox,
  showEmpty: showEmpty,
  clearSummary: clearSummary,
};
}(window));
