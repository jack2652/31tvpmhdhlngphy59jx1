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

// Track the chart container, not just the SVG: the spot-price hit target sits
// above it and leaving the plot for that target must also clear the readout.
// Mouse movement can exceed the display refresh rate, so process one point per frame.
function scheduleChartPointerMove(chartTarget, onMove, hideTooltip) {
  let frameId = null;
  let scheduled = false;
  let pending = null;
  const useAnimationFrame = typeof window.requestAnimationFrame === "function"
    && typeof window.cancelAnimationFrame === "function";
  const run = () => {
    frameId = null;
    scheduled = false;
    const event = pending;
    pending = null;
    if (event) onMove(event);
  };
  const schedule = (event) => {
    pending = { clientX: event.clientX, clientY: event.clientY, buttons: event.buttons || 0, pointerType: event.pointerType, target: event.target };
    if (scheduled) return;
    scheduled = true;
    frameId = useAnimationFrame ? window.requestAnimationFrame(run) : window.setTimeout(run, 16);
  };
  const cancel = () => {
    pending = null;
    scheduled = false;
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

function chanStructureTooltipDetails(structure) {
  if (!structure) return null;
  const displayPrice = (value) => value != null && value !== "" && Number.isFinite(Number(value)) ? formatMoney(Number(value)) : "未知";
  const startPrice = displayPrice(structure.startPrice);
  const endPrice = displayPrice(structure.endPrice);
  const start = structure.start || "未知";
  const end = structure.end || "未知";
  const formed = structure.formedAt ? `形成 ${structure.formedAt}` : "形成时间未知";
  const confirmation = structure.confirmedAt ? ` · 确认 ${structure.confirmedAt}` : " · 尚未确认";
  const level = structure.level == null ? "" : `第${Number(structure.level) + 1}层`;
  const signalTypes = ["一买", "一卖", "二买", "二卖", "三买", "三卖"];
  if (signalTypes.includes(structure.type)) {
    const phase = structure.signalPhase && structure.signalPhase !== structure.signalStatus ? ` · ${structure.signalPhase}` : "";
    const status = structure.signalStatus || (structure.confirmed ? "已确认" : "观察中");
    const boundary = displayPrice(structure.invalidationPrice);
    return {
      title: `${structure.type} · ${status}${phase}`,
      body: `${start} · 参考价 ${endPrice} · 失效边界 ${boundary}`,
    };
  }
  const type = structure.type === "线段" ? `${level}线段`
    : structure.type === "走势类型" ? `${level}走势类型${structure.trendKind ? " · " + structure.trendKind : ""}`
    : structure.type === "基础中枢" || /级中枢$/.test(structure.type) ? `${level}中枢` : structure.type;
  return {
    title: `${type}${["一买", "一卖", "二买", "二卖", "三买", "三卖"].includes(structure.type) ? ` · ${level}` : ""} · ${structure.confirmed ? "已确认" : "形成中"}`,
    body: `${start} ${startPrice} → ${end} ${endPrice} · ${formed}${confirmation} · ${structure.composition || "组成范围未知"}`,
  };
}

// Projection routes are painted into the canvas. Hit testing mirrors the old
// SVG stroke hit areas so hover and tap still open the scenario card.
function chanProjectionKeyAt(model, point, tolerance = 8) {
  if (!model?.projections?.length || !point) return null;
  if (point.x < model.historyRight || point.x > model.plotRight) return null;
  if (point.y < model.plotTop || point.y > model.plotBottom) return null;
  let best = null;
  let bestDistance = tolerance;
  for (const route of model.projections) {
    const points = route.points;
    if (!points || points.length < 2) continue;
    let distance = Infinity;
    for (let index = 1; index < points.length; index += 1) {
      const ax = points[index - 1].x, ay = points[index - 1].y;
      const dx = points[index].x - ax, dy = points[index].y - ay;
      const lengthSquared = dx * dx + dy * dy;
      const ratio = lengthSquared
        ? Math.max(0, Math.min(1, ((point.x - ax) * dx + (point.y - ay) * dy) / lengthSquared)) : 0;
      const offsetX = point.x - (ax + ratio * dx), offsetY = point.y - (ay + ratio * dy);
      distance = Math.min(distance, offsetX * offsetX + offsetY * offsetY);
    }
    if (distance <= bestDistance * bestDistance) { bestDistance = Math.sqrt(distance); best = route.key; }
  }
  return best;
}

// Canvas owns the candle geometry. This controller only updates the SVG hover
// layer and readout, so pointer movement never redraws the full chart.
function bindChanChartHover(target, initialBinding) {
  const svg = target.querySelector("svg");
  const layer = target.querySelector(".chan-crosshair-layer");
  const tooltip = target.querySelector(".chan-chart-tooltip");
  if (!svg || !layer || !tooltip) return null;
  const initial = initialBinding?.chart ? initialBinding : { chart: initialBinding };
  const vertical = layer.querySelector(".chart-crosshair");
  const horizontal = layer.querySelector(".chart-crosshair-h");
  const horizontalVolume = layer.querySelector(".chart-crosshair-volume");
  const horizontalMacd = layer.querySelector(".chart-crosshair-macd");
  const horizontalDmi = layer.querySelector(".chart-crosshair-dmi");
  const tagX = layer.querySelector(".chart-tag-x");
  const tagY = layer.querySelector(".chart-tag-y");
  const tagVolume = layer.querySelector(".chart-tag-volume");
  const tagMacd = layer.querySelector(".chart-tag-macd");
  const tagDmi = layer.querySelector(".chart-tag-dmi");
  let model = initial.chart;
  let scenariosVisible = initial.scenariosVisible !== false;
  let onScenarioHover = initial.onScenarioHover;
  let onScenarioLeave = initial.onScenarioLeave;
  let hoveredScenario = null;
  let activeIndex = null;
  let lastPointer = null;
  let shownCandle = null;
  let shownHeaderCandle = null;
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
  const volumeRow = document.createElement("div");
  volumeRow.className = "chart-tooltip-row chan-volume-row";
  const volumeLabel = document.createElement("span");
  volumeLabel.textContent = "成交量";
  const volumeValue = document.createElement("strong");
  volumeRow.append(volumeLabel, volumeValue);
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
  tooltip.append(volumeRow);
  tooltip.append(structureDetail);
  const hide = () => {
    const wasActive = activeIndex != null;
    layer.setAttribute("visibility", "hidden");
    tooltip.hidden = true;
    activeIndex = null;
    lastPointer = null;
    if (wasActive) {
      drawChanIndicatorHeaders(target, model, latestIndicatorCandle(model));
      shownHeaderCandle = null;
    }
  };
  const hideScenarioHover = (event) => {
    if (hoveredScenario == null) return;
    hoveredScenario = null;
    if (target.style) target.style.cursor = "";
    onScenarioLeave?.(event || { type: "pointerleave" });
  };
  const setScenarioHover = (key, event) => {
    if (key === hoveredScenario) return;
    if (!key) { hideScenarioHover(event); return; }
    hoveredScenario = key;
    if (target.style) target.style.cursor = "pointer";
    onScenarioHover?.(key, event);
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
    if (shownHeaderCandle !== candle) {
      drawChanIndicatorHeaders(target, model, candle);
      shownHeaderCandle = candle;
    }
    const localX = Number.isFinite(localXOverride) ? localXOverride : candle.x;
    const crosshairBottom = Number.isFinite(model.crosshairBottom) ? model.crosshairBottom : model.plotBottom;
    const inPricePane = localY >= model.plotTop && localY <= model.plotBottom;
    const inVolumePane = Number.isFinite(model.volumeTop) && Number.isFinite(model.volumeBottom)
      && localY >= model.volumeTop && localY <= model.volumeBottom;
    const inMacdPane = Number.isFinite(model.macdTop) && Number.isFinite(model.macdBottom)
      && localY >= model.macdTop && localY <= model.macdBottom;
    const inDmiPane = Number.isFinite(model.dmiTop) && Number.isFinite(model.dmiBottom)
      && localY >= model.dmiTop && localY <= model.dmiBottom;
    const plotHeight = model.plotBottom - model.plotTop;
    const priceRange = model.maximum - model.minimum;
    const cursorY = Math.max(model.plotTop, Math.min(model.plotBottom, localY));
    const price = model.maximum - (cursorY - model.plotTop) / plotHeight * priceRange;
    setLine(vertical, candle.x, model.plotTop, candle.x, crosshairBottom);
    // The Chan chart reserves its right side for scenario cards; keep the
    // historical crosshair inside the candle plot so it cannot strike text.
    if (inPricePane) {
      setLine(horizontal, model.plotLeft, cursorY, model.historyRight ?? model.plotRight, cursorY);
      horizontal.removeAttribute("visibility");
    } else {
      horizontal.setAttribute("visibility", "hidden");
    }
    if (horizontalVolume) {
      if (inVolumePane) {
        setLine(horizontalVolume, model.plotLeft, localY, model.plotRight, localY);
        horizontalVolume.removeAttribute("visibility");
      } else horizontalVolume.setAttribute("visibility", "hidden");
    }
    if (horizontalMacd) {
      if (inMacdPane) {
        setLine(horizontalMacd, model.plotLeft, localY, model.plotRight, localY);
        horizontalMacd.removeAttribute("visibility");
      } else horizontalMacd.setAttribute("visibility", "hidden");
    }
    if (horizontalDmi) {
      if (inDmiPane) {
        setLine(horizontalDmi, model.plotLeft, localY, model.plotRight, localY);
        horizontalDmi.removeAttribute("visibility");
      } else horizontalDmi.setAttribute("visibility", "hidden");
    }
    const displayDate = candle.displayDate || candle.date;
    const axisDate = displayDate.includes(" ") ? displayDate.slice(5) : displayDate;
    const dateWidth = Math.max(84, axisDate.length * 7 + 12);
    setTag(tagX, axisDate, Math.max(2, Math.min(model.width - dateWidth - 2, candle.x - dateWidth / 2)), model.height - 22, dateWidth);
    const priceLabel = formatMoney(price), priceWidth = Math.min(model.plotLeft - 4, Math.max(42, priceLabel.length * 6 + 10));
    // Preserve the current-price badge when the cursor reaches the same price.
    const overlapsSpot = inPricePane && model.spot && Math.abs(cursorY - model.spot.y) < model.spot.height / 2 + 10;
    const priceX = Math.max(2, model.plotLeft - priceWidth - 4);
    setTag(tagY, priceLabel, priceX, cursorY - 9, priceWidth);
    if (!inPricePane || overlapsSpot) tagY.setAttribute("visibility", "hidden");
    else tagY.removeAttribute("visibility");
    const setPaneTag = (tag, active, text, y) => {
      if (!tag) return;
      if (!active) { tag.setAttribute("visibility", "hidden"); return; }
      const width = Math.min(model.plotLeft - 4, Math.max(42, text.length * 6 + 10));
      setTag(tag, text, Math.max(2, model.plotLeft - width - 4), y - 9, width);
      tag.removeAttribute("visibility");
    };
    const volumeAtCursor = inVolumePane && model.volumeMax > 0
      ? model.volumeMax * (model.volumeBottom - localY) / Math.max(1, model.volumeBottom - model.volumeTop) : null;
    const macdAtCursor = inMacdPane
      ? model.macdMax - (localY - model.macdTop) / Math.max(1, model.macdBottom - model.macdTop) * (model.macdMax - model.macdMin) : null;
    const dmiAtCursor = inDmiPane
      ? 100 * (model.dmiBottom - localY) / Math.max(1, model.dmiBottom - model.dmiTop) : null;
    setPaneTag(tagVolume, inVolumePane, volumeAtCursor == null ? "" : formatVolume(Math.max(0, volumeAtCursor)), localY);
    setPaneTag(tagMacd, inMacdPane, macdAtCursor == null ? "" : formatIndicator(macdAtCursor), localY);
    setPaneTag(tagDmi, inDmiPane, dmiAtCursor == null ? "" : formatIndicator(dmiAtCursor), localY);
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
      volumeValue.textContent = formatVolume(candle.volume);
      shownCandle = candle;
    }
    const distanceToSegment = (item) => {
      const dx = item.x2 - item.x1, dy = item.y2 - item.y1;
      const lengthSquared = dx * dx + dy * dy;
      const ratio = lengthSquared ? Math.max(0, Math.min(1, ((localX - item.x1) * dx + (localY - item.y1) * dy) / lengthSquared)) : 0;
      return Math.hypot(localX - (item.x1 + ratio * dx), localY - (item.y1 + ratio * dy));
    };
    const signalHit = (model.signals || []).map((signal) => {
      const markerDistance = Math.hypot(localX - signal.x, localY - (Number.isFinite(signal.markerY) ? signal.markerY : signal.y));
      const halfLabel = (signal.hitLabelWidth || 48) / 2;
      const labelX = Number.isFinite(signal.labelX) ? signal.labelX : signal.x;
      const labelDistance = localY >= signal.labelY - 12 && localY <= signal.labelY + 4 && Math.abs(localX - labelX) <= halfLabel ? 0 : Infinity;
      return { signal, distance: Math.min(markerDistance, labelDistance) };
    }).filter(({ signal, distance }) => model.layers?.signals === true && distance <= 12)
      .sort((left, right) => left.distance - right.distance)[0]?.signal;
    if (signalHit) {
      structureTitle.textContent = `${signalHit.type} · ${signalHit.signalStatus}${signalHit.signalPhase && signalHit.signalPhase !== signalHit.signalStatus ? ` · ${signalHit.signalPhase}` : ""}`;
      structureBody.textContent = `${signalHit.start} · 参考价 ${formatMoney(signalHit.endPrice)} · 失效边界 ${signalHit.invalidationPrice == null ? "未知" : formatMoney(signalHit.invalidationPrice)}`;
      structureDetail.hidden = false;
    }
    const structure = (model.structures || []).map((item) => {
      const inArea = item.area && localX >= Math.min(item.x1, item.x2) && localX <= Math.max(item.x1, item.x2)
        && localY >= Math.min(item.y1, item.y2) && localY <= Math.max(item.y1, item.y2);
      return { item, distance: item.area ? (inArea ? 5 : Infinity) : distanceToSegment(item) };
    })
      .filter(({ item, distance }) => distance <= (item.area ? 0 : 8))
      .sort((left, right) => left.distance - right.distance)[0]?.item;
    if (!signalHit && structure) {
      const detail = chanStructureTooltipDetails(structure);
      structureTitle.textContent = detail.title;
      structureBody.textContent = detail.body;
      structureDetail.hidden = false;
    } else if (!signalHit) {
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
    if (event.target?.closest?.(".chan-spot-canvas-hitbox, .chan-scenario-toggle-canvas")) { hide(); hideScenarioHover(event); return; }
    if (!model?.candles?.length || (event.buttons & 2)) { hide(); hideScenarioHover(event); return; }
    const point = localPoint(event.clientX, event.clientY);
    if (!point) { hide(); return; }
    setScenarioHover(
      scenariosVisible && event.pointerType === "mouse" && !event.buttons ? chanProjectionKeyAt(model, point) : null,
      event,
    );
    if (point.x < model.plotLeft || point.x > (model.historyRight ?? model.plotRight) || point.y < model.plotTop || point.y > (model.crosshairBottom ?? model.plotBottom)) { hide(); return; }
    lastPointer = event;
    const slot = ((model.historyRight ?? model.plotRight) - model.plotLeft) / model.candles.length;
    const index = Math.max(0, Math.min(model.candles.length - 1, Math.floor((point.x - model.plotLeft) / slot)));
    show(index, point.y, point.x);
  };
  const cancelPointerMove = scheduleChartPointerMove(target, onMove, () => { hide(); hideScenarioHover(); });
  const showClose = (index) => {
    const close = model?.candles?.[index]?.prices.close;
    if (close == null) return;
    lastPointer = null;
    show(index, model.plotTop + (model.maximum - close) / (model.maximum - model.minimum) * (model.plotBottom - model.plotTop));
  };
  const onBlur = () => { cancelPointerMove(); hide(); hideScenarioHover(); };
  const onKey = (event) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End", "Escape"].includes(event.key)) return;
    event.preventDefault();
    cancelPointerMove();
    if (event.key === "Escape") { hide(); hideScenarioHover(); return; }
    const last = model.candles.length - 1;
    const current = activeIndex ?? last;
    const next = event.key === "Home" ? 0 : event.key === "End" ? last : current + (event.key === "ArrowLeft" ? -1 : 1);
    showClose(Math.max(0, Math.min(last, next)));
  };
  const onDown = (event) => {
    cancelPointerMove();
    if (event.button === 2) { hide(); hideScenarioHover(); return; }
    onMove(event);
  };
  svg.addEventListener("blur", onBlur);
  svg.addEventListener("keydown", onKey);
  svg.addEventListener("pointerdown", onDown);
  return {
    svg,
    update(nextBinding) {
      const next = nextBinding?.chart ? nextBinding : { chart: nextBinding };
      scenariosVisible = next.scenariosVisible !== false;
      onScenarioHover = next.onScenarioHover;
      onScenarioLeave = next.onScenarioLeave;
      if (!scenariosVisible) hideScenarioHover();
      const activeDate = activeIndex == null ? null : model?.candles?.[activeIndex]?.date;
      model = next.chart;
      shownHeaderCandle = null;
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
      hoveredScenario = null;
      if (target.style) target.style.cursor = "";
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

// The scenario toggle is painted into the canvas, but its glyph comes from the
// Element UI icon font. Load that font explicitly and repaint once it arrives;
// until then a plain question mark keeps the control legible.
let elementIconFontReady = false;
let elementIconFontAttempted = false;
function ensureElementIconFont(render) {
  if (elementIconFontReady || elementIconFontAttempted) return;
  elementIconFontAttempted = true;
  if (typeof document === "undefined" || !document.fonts?.load) return;
  document.fonts.load("11px element-icons").then(() => {
    elementIconFontReady = document.fonts.check("11px element-icons");
    render();
  }).catch(() => { /* Keep the plain question-mark fallback. */ });
}

// Chart-local volume shorthand: keeps the indicator header and tooltip compact
// without pulling the page-level formatters into this module.
function formatVolume(value) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  const abs = Math.abs(number);
  if (abs >= 1e8) return (number / 1e8).toFixed(2) + "亿";
  if (abs >= 1e4) return (number / 1e4).toFixed(2) + "万";
  return String(Math.round(number));
}

function formatIndicator(value) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  return `${number > 0 ? "+" : ""}${number.toFixed(2)}`;
}

function latestIndicatorCandle(model) {
  const candles = model?.candles || [];
  return candles.length ? candles[candles.length - 1] : null;
}

function compactIndicatorValue(value) {
  if (value == null || value === "") return "--";
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  return `${number > 0 ? "+" : ""}${number.toFixed(1)}`;
}

function macdInterpretation(macd) {
  if (!macd || !Number.isFinite(macd.hist)) return "指标预热中";
  if (Math.abs(macd.hist) < 0.005) return "DIF≈DEA，动能趋平";
  if (macd.hist > 0 && macd.dif < 0 && macd.dea < 0) return "DIF>DEA，零轴下多头动能回升";
  if (macd.hist < 0 && macd.dif > 0 && macd.dea > 0) return "DIF<DEA，零轴上空头动能转强";
  return macd.hist > 0
    ? "DIF>DEA，柱为正，多头动能占优"
    : "DIF<DEA，柱为负，空头动能占优";
}

function compactMacdInterpretation(macd) {
  if (!macd || !Number.isFinite(macd.hist) || Math.abs(macd.hist) < 0.005) return "动能趋平";
  if (macd.hist > 0 && macd.dif < 0 && macd.dea < 0) return "零轴下多头回升";
  if (macd.hist < 0 && macd.dif > 0 && macd.dea > 0) return "零轴上空头转强";
  return macd.hist > 0 ? "动能偏多" : "动能偏空";
}

function indicatorHeaderText(candle) {
  const volume = candle && Number.isFinite(Number(candle.volume)) ? formatVolume(candle.volume) : "--";
  const macd = candle?.macd;
  const dmi = candle?.dmi;
  const balanced = dmi && Math.abs(dmi.difference) < 0.01;
  const dmiLeader = !dmi ? "" : balanced
    ? "多空均衡" : dmi.difference > 0 ? "多头" : "空头";
  const adx = Number.isFinite(dmi?.adx) ? formatIndicator(dmi.adx) : "预热中";
  const dmiStatus = !dmi ? "预热中"
    : balanced ? "方向胶着"
      : !Number.isFinite(dmi.adx) ? `${dmiLeader}占优`
        : dmi.adx > 25 ? `${dmiLeader}趋势强` : `${dmiLeader}占优`;
  const dmiInterpretation = !dmi ? "DMI指标预热中"
    : !Number.isFinite(dmi.adx) ? `${balanced ? "PDI≈MDI，方向胶着" : dmi.difference > 0 ? "PDI>MDI，多头占优" : "MDI>PDI，空头占优"}，ADX预热中`
      : balanced ? dmi.adx > 25 ? "PDI≈MDI，方向胶着；ADX>25，趋势强" : "PDI≈MDI，方向不明；ADX≤25，趋势偏弱"
        : `${dmi.difference > 0 ? "PDI>MDI，多头" : "MDI>PDI，空头"}主导；ADX${dmi.adx > 25 ? ">25，趋势强" : "≤25，趋势偏弱"}`;
  return {
    volume: `成交量  ${volume}`,
    macd: macd
      ? `MACD  DIF ${formatIndicator(macd.dif)} · DEA ${formatIndicator(macd.dea)} · 柱 ${formatIndicator(macd.hist)}`
      : "MACD",
    macdCompact: macd
      ? `MACD DIF ${compactIndicatorValue(macd.dif)} · DEA ${compactIndicatorValue(macd.dea)} · 柱 ${compactIndicatorValue(macd.hist)}`
      : "MACD",
    macdInterpretation: macdInterpretation(macd),
    macdCompactInterpretation: compactMacdInterpretation(macd),
    dmi: dmi
      ? `DMI  PDI ${formatIndicator(dmi.pdi)} · MDI ${formatIndicator(dmi.mdi)} · ADX ${adx}`
      : "DMI(14,6) 预热中",
    dmiCompact: dmi
      ? `DMI PDI ${compactIndicatorValue(dmi.pdi)} · MDI ${compactIndicatorValue(dmi.mdi)} · ADX ${compactIndicatorValue(dmi.adx)}`
      : "DMI(14,6) 预热中",
    dmiStatus,
    dmiInterpretation,
  };
}

function drawInlineIndicatorHeader(context, model, x, y, width, text, insight, compactText, compactInsight, textColor, insightColor) {
  const mobile = Boolean(model?.mobileChart);
  const candidates = mobile
    ? [
      { font: "14px Inter, ui-sans-serif, system-ui, sans-serif", text, insight },
      { font: "12px Inter, ui-sans-serif, system-ui, sans-serif", text: compactText, insight: compactInsight },
    ]
    : [
      { font: "9px Inter, ui-sans-serif, system-ui, sans-serif", text, insight },
      { font: "8px Inter, ui-sans-serif, system-ui, sans-serif", text: compactText, insight: compactInsight },
    ];
  context.textAlign = "left";
  context.textBaseline = "alphabetic";
  if (typeof context.measureText !== "function") {
    context.font = indicatorHeaderFont(model);
    context.fillStyle = textColor;
    context.fillText(text, x, y, width);
    return;
  }
  const gap = mobile ? 8 : 10;
  for (const candidate of candidates) {
    context.font = candidate.font;
    const textWidth = context.measureText(candidate.text).width;
    const insightWidth = candidate.insight ? context.measureText(candidate.insight).width : 0;
    if (textWidth + (insightWidth ? gap + insightWidth : 0) > width) continue;
    context.fillStyle = textColor;
    context.fillText(candidate.text, x, y);
    if (insightWidth) {
      context.fillStyle = insightColor;
      context.fillText(candidate.insight, x + textWidth + gap, y);
    }
    return;
  }
  const fallback = candidates[candidates.length - 1];
  context.font = fallback.font;
  context.fillStyle = textColor;
  context.fillText(fallback.text, x, y, width);
}

function indicatorHeaderFont(model) {
  // Mobile charts paint a 420px model into a narrower card, so the indicator
  // headers use the same scale as the mobile price and date labels.
  return `${model?.mobileChart ? 14 : 9}px Inter, ui-sans-serif, system-ui, sans-serif`;
}

// Repaint only the two header strips while hovering. Clearing the whole chart
// on every pointer move would defeat the canvas hover fast path.
function drawChanIndicatorHeaders(target, model, candle) {
  const canvas = target.querySelector("canvas.chan-chart-canvas");
  const context = canvas?.getContext("2d");
  if (!context || !model || !(canvas.width > 0) || !(canvas.height > 0)
    || !Number.isFinite(model.volumeTop) || !Number.isFinite(model.macdTop)) return;
  const svg = target.querySelector("svg");
  const chartWidth = svg?.getBoundingClientRect().width || 0;
  const leftBleed = Number(canvas.dataset.leftBleed) || 0;
  const pixelScale = canvas.width / (chartWidth + leftBleed);
  context.setTransform(pixelScale * chartWidth / model.width, 0, 0, canvas.height / model.height,
    pixelScale * leftBleed, 0);
  const color = chanThemeColor(target, "--muted", "#81909c");
  const width = Math.max(1, model.plotRight - model.plotLeft + 4);
  context.clearRect(model.plotLeft - 2, model.volumeTop - 18, width, 16);
  context.clearRect(model.plotLeft - 2, model.macdTop - 18, width, 16);
  context.font = indicatorHeaderFont(model);
  context.fillStyle = color;
  context.textAlign = "left";
  context.textBaseline = "alphabetic";
  const header = indicatorHeaderText(candle);
  context.fillText(header.volume, model.plotLeft, model.volumeTop - 6);
  const bullishColor = chanThemeColor(target, "--up", "#19bd83");
  const bearishColor = chanThemeColor(target, "--down", "#f04d68");
  const macdColor = candle?.macd?.hist > 0 ? bullishColor : candle?.macd?.hist < 0 ? bearishColor : color;
  drawInlineIndicatorHeader(context, model, model.plotLeft, model.macdTop - 6, width,
    header.macd, header.macdInterpretation, header.macdCompact, header.macdCompactInterpretation, color, macdColor);
  if (!Number.isFinite(model.dmiTop)) return;
  context.clearRect(model.plotLeft - 2, model.dmiTop - 18, width, 16);
  // 美股图表使用绿色表示上涨、红色表示下跌。
  const dmiDifference = Number(candle?.dmi?.difference);
  const dmiColor = !Number.isFinite(dmiDifference) || Math.abs(dmiDifference) < 0.01
    ? color : dmiDifference > 0 ? bullishColor : bearishColor;
  drawInlineIndicatorHeader(context, model, model.plotLeft, model.dmiTop - 6, width,
    header.dmi, header.dmiInterpretation, header.dmiCompact, header.dmiStatus, color, dmiColor);
}

function drawChanCanvas(target, binding) {
  // The view model has already applied per-level preferences. Keep its layer
  // flags authoritative so a legacy stored segment toggle cannot hide L0.
  const model = binding?.chart ? { ...binding.chart, layers: binding.chart.layers || binding.layers || {} } : binding;
  const scenariosVisible = binding?.chart ? binding.scenariosVisible !== false : true;
  const canvas = target.querySelector("canvas.chan-chart-canvas");
  const svg = target.querySelector("svg");
  if (!canvas || !svg || !model) return;
  const bounds = svg.getBoundingClientRect();
  const width = bounds.width, height = bounds.height;
  if (!width || !height || !model.width || !model.height) return;
  const ratio = Math.max(1, Math.min(3, window.devicePixelRatio || 1));
  const leftBleed = model.mobileChart ? 80 : 0;
  const canvasWidth = width + leftBleed;
  const pixelWidth = Math.round(canvasWidth * ratio), pixelHeight = Math.round(height * ratio);
  if (canvas.width !== pixelWidth || canvas.height !== pixelHeight) {
    canvas.width = pixelWidth; canvas.height = pixelHeight;
  }
  const context = canvas.getContext("2d");
  if (!context) return;
  canvas.dataset.leftBleed = String(leftBleed);
  canvas.style.left = `${-leftBleed}px`; canvas.style.top = "0";
  canvas.style.width = `${canvasWidth}px`; canvas.style.height = `${height}px`;
  const pixelScale = pixelWidth / canvasWidth;
  context.setTransform(1, 0, 0, 1, 0, 0);
  context.clearRect(0, 0, pixelWidth, pixelHeight);
  context.setTransform(pixelScale * width / model.width, 0, 0, pixelHeight / model.height,
    pixelScale * leftBleed, 0);
  ensureElementIconFont(() => drawChanCanvas(target, binding));
  const colors = {
    grid: chanThemeColor(target, "--grid", "#344654"), axis: chanThemeColor(target, "--axis", "#81909c"),
    text: chanThemeColor(target, "--muted", "#81909c"), textPrimary: chanThemeColor(target, "--text", "#e8eef3"), panel: chanThemeColor(target, "--panel", "#101923"),
    up: chanThemeColor(target, "--up", "#19bd83"), down: chanThemeColor(target, "--down", "#f04d68"),
    blue: chanThemeColor(target, "--blue", "#69b7ff"), spot: chanThemeColor(target, "--muted", "#81909c"),
    field: chanThemeColor(target, "--field", "#0b141d"), line: chanThemeColor(target, "--line", "#253543"),
    fieldLine: chanThemeColor(target, "--field-line", "#3b5263"),
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
    if (model.layers?.recursiveCenters?.[zone.level] === false) continue;
    const color = levelColor(zone.level);
    context.fillStyle = color; context.globalAlpha = zone.forming ? 0.045 : 0.085;
    context.fillRect(zone.x, zone.y, zone.width, zone.height);
    context.strokeStyle = color; context.globalAlpha = zone.forming ? 0.72 : 0.46;
    context.lineWidth = Math.min(2.6, 1 + (zone.extensionCount || 0) * 0.18);
    context.setLineDash(zone.forming ? [5, 3] : levelDash(zone.level)); context.strokeRect(zone.x, zone.y, zone.width, zone.height); context.setLineDash([]);
    if (zone.width > 34 && zone.height > 12) label("第" + (Number(zone.level) + 1) + "层", zone.x + 4, zone.y + 10, { font: "9px Inter, ui-sans-serif, system-ui, sans-serif", color });
    context.globalAlpha = 1;
  }
  // Keep each original candle body in its market color. Each merged Chan
  // candle is shown once, anchored at the last source slot in its range, with
  // an identically sized pale column behind that body's real high-low range.
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
  if (model.layers?.pens === true) for (const pen of model.strokes || []) line(pen.x1, pen.y1, pen.x2, pen.y2, colors.blue, pen.confirmed ? [] : [6, 4], 2.2);
  if (model.layers?.pens === true) for (const point of model.points || []) {
    context.beginPath(); context.arc(point.x, point.y, 2.6, 0, Math.PI * 2);
    context.fillStyle = point.confirmed ? colors.blue : colors.panel; context.fill();
    context.strokeStyle = colors.blue; context.lineWidth = point.confirmed ? 1 : 1.5; context.stroke();
    context.font = model.mobileChart ? "550 12px Inter, ui-sans-serif, system-ui, sans-serif" : "9px Inter, ui-sans-serif, system-ui, sans-serif";
    context.textAlign = "center"; context.textBaseline = "alphabetic";
    context.lineWidth = 3; context.strokeStyle = colors.panel; context.lineJoin = "round";
    context.strokeText(point.label, point.labelX, point.labelY); context.fillStyle = colors.textPrimary; context.fillText(point.label, point.labelX, point.labelY);
  }
  if (model.layers?.legacySignals === true) for (const marker of model.markers || []) {
    context.beginPath(); context.arc(marker.x, marker.y, 5, 0, Math.PI * 2);
    const markerColor = marker.invalidated ? colors.text : colors.blue;
    context.fillStyle = colors.panel; context.strokeStyle = markerColor; context.lineWidth = 1.5; context.setLineDash(marker.watch || marker.invalidated ? [2, 2] : []); context.fill(); context.stroke(); context.setLineDash([]);
    context.font = model.mobileChart ? "550 12px Inter, ui-sans-serif, system-ui, sans-serif" : "9px Inter, ui-sans-serif, system-ui, sans-serif"; context.textAlign = "center";
    context.lineWidth = 3; context.strokeStyle = colors.panel; context.strokeText(marker.label, marker.labelX, marker.labelY);
    context.fillStyle = markerColor; context.fillText(marker.label, marker.labelX, marker.labelY);
  }
  // Chan structural reference levels: recent swing highs/lows and the latest center boundaries.
  for (const level of model.keyLevels || []) {
    const color = level.tone === "support" ? colors.up : level.tone === "resistance" ? colors.down : colors.blue;
    line(level.x1, level.y, level.x2, level.y, color, [3, 4], 1);
  }
  if (model.layers?.signals === true) for (const signal of model.signals || []) {
    const color = signal.invalidated ? colors.text : signal.side === "buy" ? colors.up : colors.down;
    const markerY = Number.isFinite(signal.markerY) ? signal.markerY : signal.y;
    context.beginPath();
    context.moveTo(signal.x, markerY + (signal.side === "buy" ? 6 : -6));
    context.lineTo(signal.x - 5, markerY + (signal.side === "buy" ? -2 : 2));
    context.lineTo(signal.x + 5, markerY + (signal.side === "buy" ? -2 : 2));
    context.closePath();
    context.fillStyle = colors.panel; context.strokeStyle = color; context.lineWidth = signal.invalidated ? 1.5 : 0;
    context.setLineDash(signal.invalidated ? [2, 2] : []);
    if (signal.invalidated) context.stroke();
    else { context.fillStyle = color; context.globalAlpha = signal.status === "confirmed" ? 0.95 : 0.64; context.fill(); context.globalAlpha = 1; }
    context.setLineDash([]);
    if (signal.invalidated) {
      context.beginPath(); context.moveTo(signal.x - 3, markerY - 3); context.lineTo(signal.x + 3, markerY + 3);
      context.moveTo(signal.x + 3, markerY - 3); context.lineTo(signal.x - 3, markerY + 3);
      context.strokeStyle = color; context.lineWidth = 1.2; context.stroke();
    }
    const status = signal.invalidated ? "" : signal.status === "confirmed" ? "✓" : "?";
    const signalLabel = signal.label + status;
    const labelY = Number.isFinite(signal.labelY) ? signal.labelY : signal.y + (signal.side === "buy" ? 16 : -10);
    context.font = model.mobileChart ? "550 12px Inter, ui-sans-serif, system-ui, sans-serif" : "9px Inter, ui-sans-serif, system-ui, sans-serif"; context.textAlign = "center"; context.textBaseline = "alphabetic";
    const labelWidth = context.measureText(signalLabel).width;
    const labelMinX = model.plotLeft + labelWidth / 2 + 2;
    const labelMaxX = model.historyRight - labelWidth / 2 - 2;
    const preferredLabelX = Number.isFinite(signal.labelX) ? signal.labelX : signal.x;
    const labelX = labelMinX <= labelMaxX
      ? Math.max(labelMinX, Math.min(labelMaxX, preferredLabelX))
      : (model.plotLeft + model.historyRight) / 2;
    context.lineWidth = 3; context.lineJoin = "round"; context.strokeStyle = colors.panel; context.strokeText(signalLabel, labelX, labelY);
    context.fillStyle = color; context.fillText(signalLabel, labelX, labelY);
  }
  if (model.spot) {
    line(model.plotLeft, model.spot.y, model.historyRight, model.spot.y, colors.spot, [3, 4], 1);
    // Badge geometry is expressed in CSS pixels here. The hitbox and the former
    // DOM badge both use fixed CSS dimensions, so only the chart coordinates
    // above need the model-to-screen scale.
    context.save();
    context.setTransform(pixelScale, 0, 0, pixelHeight / height, pixelScale * leftBleed, 0);
    const spotPadding = model.mobileChart ? 3 : 4;
    const spotHeight = model.spot.height || 22;
    const spotY = model.spot.y * height / model.height - spotHeight / 2;
    const spotRight = model.plotLeft * width / model.width - 4;
    const spotFontSize = 13;
    context.font = `750 ${spotFontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
    const spotWidth = context.measureText(model.spot.label).width + spotPadding * 2 + 2;
    const spotHitbox = target.querySelector(".chan-spot-canvas-hitbox");
    if (spotHitbox) {
      spotHitbox.style.width = `${spotWidth}px`;
      spotHitbox.style.height = `${spotHeight}px`;
    }
    const spotX = spotRight - spotWidth;
    const radius = 3;
    context.beginPath();
    context.moveTo(spotX + radius, spotY);
    context.lineTo(spotX + spotWidth - radius, spotY);
    context.arcTo(spotX + spotWidth, spotY, spotX + spotWidth, spotY + radius, radius);
    context.lineTo(spotX + spotWidth, spotY + spotHeight - radius);
    context.arcTo(spotX + spotWidth, spotY + spotHeight, spotX + spotWidth - radius, spotY + spotHeight, radius);
    context.lineTo(spotX + radius, spotY + spotHeight);
    context.arcTo(spotX, spotY + spotHeight, spotX, spotY + spotHeight - radius, radius);
    context.lineTo(spotX, spotY + radius);
    context.arcTo(spotX, spotY, spotX + radius, spotY, radius);
    context.closePath();
    context.fillStyle = colors.panel;
    context.fill();
    context.strokeStyle = colors.blue;
    context.lineWidth = 1;
    context.stroke();
    context.fillStyle = colors.blue;
    context.textAlign = "left";
    context.textBaseline = "middle";
    context.fillText(model.spot.label, spotX + spotPadding + 1, spotY + spotHeight / 2);
    context.restore();
  }
  // Volume pane: same time axis and candle slot width as the price pane.
  if (Number.isFinite(model.volumeTop) && Number.isFinite(model.volumeBottom)) {
    const volumeHeight = model.volumeBottom - model.volumeTop;
    line(model.plotLeft, model.volumeTop, model.plotRight, model.volumeTop, colors.grid, [2, 4], 1);
    context.save();
    context.globalAlpha = 0.72;
    for (const candle of model.candles || []) {
      const volume = Number(candle.volume) || 0;
      if (volume <= 0) continue;
      const barHeight = Math.max(1, Math.min(volumeHeight - 4, volume / model.volumeMax * (volumeHeight - 4)));
      context.fillStyle = candle.direction === "up" ? colors.up : colors.down;
      context.fillRect(candle.x - candle.bodyWidth / 2, model.volumeBottom - barHeight, Math.max(1, candle.bodyWidth), barHeight);
    }
    context.restore();
    const indicatorFont = indicatorHeaderFont(model);
    label(indicatorHeaderText(latestIndicatorCandle(model)).volume, model.plotLeft, model.volumeTop - 6,
      { font: indicatorFont, color: colors.text });
    line(model.plotLeft, model.volumeBottom, model.plotRight, model.volumeBottom, colors.axis, [], 1);
  }
  // Keep the MACD pane visually separate from the volume block; the MACD header
  // sits in the gap and would otherwise look like the volume pane's footer.
  if (Number.isFinite(model.volumeBottom) && Number.isFinite(model.macdTop)) {
    const dividerY = Number.isFinite(model.indicatorDividerY)
      ? model.indicatorDividerY
      : model.volumeBottom + (model.macdTop - model.volumeBottom) / 3;
    line(model.plotLeft, dividerY, model.plotRight, dividerY, colors.fieldLine, [], 1.5);
  }
  // MACD pane: histogram around zero plus DIF/DEA lines.
  if (Number.isFinite(model.macdTop) && Number.isFinite(model.macdBottom)) {
    const macdHeight = model.macdBottom - model.macdTop;
    const macdY = (value) => model.macdTop + (model.macdMax - value) / (model.macdMax - model.macdMin) * macdHeight;
    const zeroY = Number.isFinite(model.macdZeroY) ? model.macdZeroY : macdY(0);
    line(model.plotLeft, model.macdTop, model.plotRight, model.macdTop, colors.grid, [2, 4], 1);
    context.save();
    for (const candle of model.candles || []) {
      const item = candle.macd;
      if (!item || !Number.isFinite(item.hist)) continue;
      const valueY = macdY(item.hist);
      context.globalAlpha = 0.62;
      context.fillStyle = item.hist >= 0 ? colors.up : colors.down;
      context.fillRect(candle.x - candle.bodyWidth / 2, Math.min(zeroY, valueY), Math.max(1, candle.bodyWidth), Math.max(1, Math.abs(valueY - zeroY)));
    }
    context.restore();
    line(model.plotLeft, zeroY, model.plotRight, zeroY, colors.axis, [3, 3], 1);
    const drawMacdLine = (key, color) => {
      context.beginPath();
      let started = false;
      for (const candle of model.candles || []) {
        const item = candle.macd;
        if (!item || !Number.isFinite(item[key])) { started = false; continue; }
        const valueY = macdY(item[key]);
        if (started) context.lineTo(candle.x, valueY);
        else { context.moveTo(candle.x, valueY); started = true; }
      }
      context.strokeStyle = color;
      context.lineWidth = 1.4;
      context.setLineDash([]);
      context.stroke();
    };
    drawMacdLine("dif", colors.amber);
    drawMacdLine("dea", colors.blue);
    const latestCandle = latestIndicatorCandle(model);
    const header = indicatorHeaderText(latestCandle);
    const macdColor = latestCandle?.macd?.hist > 0 ? colors.up : latestCandle?.macd?.hist < 0 ? colors.down : colors.text;
    drawInlineIndicatorHeader(context, model, model.plotLeft, model.macdTop - 6,
      Math.max(1, model.plotRight - model.plotLeft), header.macd, header.macdInterpretation,
      header.macdCompact, header.macdCompactInterpretation, colors.text, macdColor);
    line(model.plotLeft, model.macdBottom, model.plotRight, model.macdBottom, colors.axis, [], 1);
  }
  if (Number.isFinite(model.macdBottom) && Number.isFinite(model.dmiTop)) {
    const dividerY = Number.isFinite(model.dmiDividerY)
      ? model.dmiDividerY
      : model.macdBottom + (model.dmiTop - model.macdBottom) / 3;
    line(model.plotLeft, dividerY, model.plotRight, dividerY, colors.fieldLine, [], 1.5);
  }
  // DMI uses a fixed 0-100 range so PDI, MDI, ADX, and the 25 threshold remain comparable.
  if (Number.isFinite(model.dmiTop) && Number.isFinite(model.dmiBottom)) {
    const dmiHeight = model.dmiBottom - model.dmiTop;
    const dmiY = (value) => model.dmiTop + (100 - value) / 100 * dmiHeight;
    // 美股图表使用绿色表示上涨、红色表示下跌。
    const bullishColor = colors.up;
    const bearishColor = colors.down;
    line(model.plotLeft, model.dmiTop, model.plotRight, model.dmiTop, colors.grid, [2, 4], 1);
    for (const value of [0, 25, 50, 75, 100]) {
      const y = dmiY(value);
      if (value !== 25) line(model.plotLeft, y, model.plotRight, y, colors.grid, [2, 4], 1);
      label(String(value), model.plotLeft - 8, value === 0 ? y - 2 : y + 3, {
        align: "right", font: `${model.mobileChart ? 11 : 9}px Inter, ui-sans-serif, system-ui, sans-serif`,
        color: value === 25 ? colors.amber : colors.text,
      });
    }
    context.save();
    context.globalAlpha = 0.16;
    const candles = model.candles || [];
    for (let index = 1; index < candles.length; index += 1) {
      const previous = candles[index - 1]?.dmi, current = candles[index]?.dmi;
      if (!previous || !current) continue;
      context.beginPath();
      context.moveTo(candles[index - 1].x, dmiY(previous.pdi));
      context.lineTo(candles[index].x, dmiY(current.pdi));
      context.lineTo(candles[index].x, dmiY(current.mdi));
      context.lineTo(candles[index - 1].x, dmiY(previous.mdi));
      context.closePath();
      context.fillStyle = current.difference >= 0 ? bullishColor : bearishColor;
      context.fill();
    }
    context.restore();
    const drawDmiLine = (key, color, lineWidth) => {
      context.beginPath();
      let started = false;
      for (const candle of candles) {
        const value = candle.dmi?.[key];
        if (!Number.isFinite(value)) { started = false; continue; }
        const y = dmiY(value);
        if (started) context.lineTo(candle.x, y);
        else { context.moveTo(candle.x, y); started = true; }
      }
      context.strokeStyle = color;
      context.lineWidth = lineWidth;
      context.setLineDash([]);
      context.stroke();
    };
    drawDmiLine("pdi", bullishColor, 1.5);
    drawDmiLine("mdi", bearishColor, 1.5);
    const drawAdxLine = () => {
      let previous = null;
      for (const candle of candles) {
        const value = candle.dmi?.adx;
        if (!Number.isFinite(value)) { previous = null; continue; }
        const current = { x: candle.x, value, y: dmiY(value) };
        if (previous) {
          const previousAbove = previous.value > 25, currentAbove = current.value > 25;
          context.beginPath();
          context.moveTo(previous.x, previous.y);
          if (previousAbove !== currentAbove) {
            const ratio = (25 - previous.value) / (current.value - previous.value);
            const crossing = { x: previous.x + (current.x - previous.x) * ratio, y: dmiY(25) };
            context.lineTo(crossing.x, crossing.y);
            context.strokeStyle = previousAbove ? colors.amber : colors.text;
            context.lineWidth = 1.8;
            context.stroke();
            context.beginPath();
            context.moveTo(crossing.x, crossing.y);
          }
          context.lineTo(current.x, current.y);
          context.strokeStyle = currentAbove ? colors.amber : colors.text;
          context.lineWidth = 1.8;
          context.setLineDash([]);
          context.stroke();
        }
        previous = current;
      }
    };
    drawAdxLine();
    line(model.plotLeft, dmiY(25), model.plotRight, dmiY(25), colors.amber, [4, 3], 1.2);
    const latestCandle = latestIndicatorCandle(model);
    const header = indicatorHeaderText(latestCandle);
    const difference = Number(latestCandle?.dmi?.difference);
    const dmiColor = !Number.isFinite(difference) || Math.abs(difference) < 0.01
      ? colors.text : difference > 0 ? bullishColor : bearishColor;
    drawInlineIndicatorHeader(context, model, model.plotLeft, model.dmiTop - 6,
      Math.max(1, model.plotRight - model.plotLeft), header.dmi, header.dmiInterpretation,
      header.dmiCompact, header.dmiStatus, colors.text, dmiColor);
    line(model.plotLeft, model.dmiBottom, model.plotRight, model.dmiBottom, colors.axis, [], 1);
  }
  // Bottom axis and the three visible date anchors.
  line(model.plotLeft, model.plotBottom, model.plotRight, model.plotBottom, colors.axis, [], 1);
  for (const date of model.dates || []) {
    const align = date.x < model.width * .1 ? "left" : date.x > model.width * .9 ? "right" : "center";
    label(date.label, date.x, date.y, { align });
  }
  // Conditional scenarios share the same canvas as the history layer. Their
  // geometry is already projected into chart coordinates by the view model.
  if (scenariosVisible && model.projections?.length) {
    context.save();
    const laneLeft = Number.isFinite(model.scenarioLaneLeft) ? model.scenarioLaneLeft : model.historyRight;
    const laneWidth = model.plotRight - laneLeft;
    if (laneWidth > 0) {
      context.globalAlpha = 0.92;
      context.fillStyle = colors.field;
      context.fillRect(laneLeft, model.plotTop, laneWidth, model.plotBottom - model.plotTop);
      context.globalAlpha = 1;
      context.strokeStyle = colors.line;
      context.lineWidth = 1;
      context.setLineDash([2, 4]);
      context.strokeRect(laneLeft + 0.5, model.plotTop + 0.5, laneWidth - 1, model.plotBottom - model.plotTop - 1);
      context.setLineDash([]);
      if (!model.mobileChart) {
        label("条件情景 · 非预测", laneLeft + 8, 31, { font: "9px Inter, ui-sans-serif, system-ui, sans-serif", color: colors.text });
      }
    }
    for (const route of model.projections) {
      const points = route.points;
      if (!points || points.length < 2) continue;
      const color = route.tone === "up" ? colors.up : route.tone === "down" ? colors.down : colors.blue;
      context.globalAlpha = 0.92;
      context.strokeStyle = color;
      context.lineWidth = 1.8;
      context.setLineDash([6, 5]);
      context.beginPath();
      context.moveTo(points[0].x, points[0].y);
      for (let index = 1; index < points.length; index += 1) context.lineTo(points[index].x, points[index].y);
      context.stroke();
      context.setLineDash([]);
      context.globalAlpha = 1;
      const last = points[points.length - 1], previous = points[points.length - 2];
      const angle = Math.atan2(last.y - previous.y, last.x - previous.x);
      context.save();
      context.translate(last.x, last.y);
      context.rotate(angle);
      context.beginPath();
      context.moveTo(-5.5, -3.5); context.lineTo(1.5, 0); context.lineTo(-5.5, 3.5); context.closePath();
      context.fillStyle = color;
      context.fill();
      context.restore();
      for (const point of points) {
        context.beginPath();
        context.arc(point.x, point.y, 3, 0, Math.PI * 2);
        context.fillStyle = colors.panel;
        context.fill();
        context.strokeStyle = color;
        context.lineWidth = 1.5;
        context.stroke();
      }
    }
    context.restore();
  }
  // Scenario visibility control. The glyph is painted here; a transparent
  // HTML button above the canvas keeps focus, keyboard and tooltip behavior.
  const toggle = target.querySelector(".chan-scenario-toggle-canvas");
  const toggleActive = Boolean(toggle?.matches?.(":hover, :focus-visible"));
  const toggleSize = model.mobileChart ? 22 : 16;
  const toggleInset = toggleSize + 3;
  const toggleX = model.historyRight - toggleInset, toggleY = model.plotBottom - toggleInset;
  context.save();
  context.globalAlpha = 1;
  context.fillStyle = colors.panel;
  context.fillRect(toggleX, toggleY, toggleSize, toggleSize);
  context.strokeStyle = toggleActive ? colors.blue : colors.fieldLine;
  context.lineWidth = 0.8;
  context.strokeRect(toggleX, toggleY, toggleSize, toggleSize);
  context.fillStyle = toggleActive ? colors.blue : colors.text;
  context.textAlign = "center";
  context.textBaseline = "middle";
  if (elementIconFontReady) {
    context.font = `${model.mobileChart ? 13 : 11}px element-icons`;
    context.fillText("\ue7b3", toggleX + toggleSize / 2, toggleY + toggleSize / 2);
  } else {
    context.font = `700 ${model.mobileChart ? 12 : 10}px Inter, ui-sans-serif, system-ui, sans-serif`;
    context.fillText("?", toggleX + toggleSize / 2, toggleY + toggleSize / 2 + 0.5);
  }
  context.restore();
}

const chanCanvasControllers = new WeakMap();
function syncChanCanvas(element, binding) {
  let controller = chanCanvasControllers.get(element);
  if (!controller) {
    const render = () => drawChanCanvas(element, controller?.model);
    controller = { model: binding.value, render, observer: null, themeObserver: null, toggle: null, toggleHandlers: [] };
    controller.toggle = element.querySelector(".chan-scenario-toggle-canvas");
    if (controller.toggle) {
      controller.toggleHandlers = [["pointerenter", render], ["pointerleave", render], ["focus", render], ["blur", render]];
      controller.toggleHandlers.forEach(([type, handler]) => controller.toggle.addEventListener(type, handler));
    }
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
    controller.toggleHandlers?.forEach(([type, handler]) => controller.toggle?.removeEventListener(type, handler));
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
  const state = { model: binding.value?.chart, onPan: binding.value?.onPan, onZoom: binding.value?.onZoom,
    scenariosVisible: binding.value?.scenariosVisible !== false, onScenarioClick: binding.value?.onScenarioClick,
    pointers: new Map(), drag: null, pinch: null, lastPinchBars: null, suppressClick: false, clickTimer: null };
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
    // SVG controls and the scenario toggle own their click/touch gestures. Taking
    // pointer capture here retargets their click to the chart container.
    if (event.target.closest?.(".chan-scenario-toggle-canvas, .chan-projection-backdrop, .chan-projection-up, .chan-projection-down, .chan-projection-range")) return;
    const point = localPoint(event);
    const model = state.model;
    if (!point || !model || point.x < model.plotLeft || point.x > model.historyRight || point.y < model.plotTop || point.y > (model.interactionBottom ?? model.plotBottom)) return;
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
    if (state.suppressClick) {
      window.clearTimeout(state.clickTimer);
      state.clickTimer = window.setTimeout(() => { state.suppressClick = false; }, 500);
      event.preventDefault(); event.stopPropagation();
      return;
    }
    if (!state.scenariosVisible || !state.onScenarioClick || !state.model) return;
    if (event.target?.closest?.(".chan-projection-backdrop, .chan-projection-float-card")) return;
    const point = localPoint(event);
    const key = point ? chanProjectionKeyAt(state.model, point, 16) : null;
    if (!key) return;
    // The canvas has no per-route DOM node, so stop the container-level
    // dismiss handler and forward the hit before Vue sees the click.
    event.preventDefault(); event.stopPropagation();
    state.onScenarioClick(key, event);
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
    update(value) {
      state.model = value?.chart;
      state.onPan = value?.onPan;
      state.onZoom = value?.onZoom;
      state.scenariosVisible = value?.scenariosVisible !== false;
      state.onScenarioClick = value?.onScenarioClick;
    },
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
