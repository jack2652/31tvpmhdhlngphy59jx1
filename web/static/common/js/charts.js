/* SVG 图表模块：只负责绘制与交互，行情状态和接口请求由 app.js / request.js 管理。 */
(function (window) {
// 图表层独立格式化价格，避免依赖股票页才会加载的 app.js。
function moneyDecimals(value) {
  const number = Math.abs(Number(value));
  if (!Number.isFinite(number)) return 2;
  if (number >= 1) return 2;
  if (number >= 0.1) return 3;
  if (number >= 0.01) return 4;
  if (number >= 0.001) return 5;
  if (number >= 0.0001) return 6;
  if (number >= 0.00001) return 7;
  return 8;
}

function formatMoney(value) {
  if (value == null) return "--";
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  const fixed = number.toFixed(moneyDecimals(number));
  if (!fixed.includes(".")) return fixed;
  const compact = fixed.replace(/(\.\d*?[1-9])0+$/, "$1").replace(/\.0+$/, "");
  return compact.includes(".") ? compact : `${compact}.00`;
}

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
  const ruleVersion = structure.ruleVersion ? ` · 规则 ${structure.ruleVersion}` : "";
  const level = structure.level == null ? "" : `第${Number(structure.level) + 1}层`;
  const signalTypes = ["一买", "一卖", "二买", "二卖", "三买", "三卖"];
  const warningTypes = ["顶背驰预警", "底背驰预警", "反弹承压", "回踩支撑", "顶部早预警", "反弹早预警"];
  if (signalTypes.includes(structure.type) || warningTypes.includes(structure.type)) {
    const phase = structure.signalPhase && structure.signalPhase !== structure.signalStatus ? ` · ${structure.signalPhase}` : "";
    const status = structure.signalStatus || (structure.confirmed ? "已确认" : "观察中");
    const boundary = displayPrice(structure.invalidationPrice);
    // 预警确认只指本级结构，失效阶段时间不能冒充确认时间。
    const dates = warningTypes.includes(structure.type)
      ? ` · 可观察 ${structure.formedAt || "未知"} · 确认 ${structure.confirmedAt || "未确认"}` : "";
    return {
      title: `${structure.type} · ${status}${phase}`,
      body: `${start}${dates} · 参考价 ${endPrice}${structure.protectionPrice != null ? ` · 预警保护价 ${displayPrice(structure.protectionPrice)} · 参考顶部日期 ${structure.pivotDate || "未知"}` : ""} · 失效边界 ${boundary}${ruleVersion}`,
    };
  }
  const type = structure.type === "线段" ? `${level}线段`
    : structure.type === "走势类型" ? `${level}走势类型${structure.trendKind ? " · " + structure.trendKind : ""}`
    : structure.type === "基础中枢" || /级中枢$/.test(structure.type) ? `${level}中枢` : structure.type;
  return {
    title: `${type}${["一买", "一卖", "二买", "二卖", "三买", "三卖"].includes(structure.type) ? ` · ${level}` : ""} · ${structure.confirmed ? "已确认" : "形成中"}`,
    body: `${start} ${startPrice} → ${end} ${endPrice} · ${formed}${confirmation} · ${structure.composition || "组成范围未知"}${ruleVersion}`,
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
  const horizontalMfi = layer.querySelector(".chart-crosshair-mfi");
  const tagX = layer.querySelector(".chart-tag-x");
  const tagY = layer.querySelector(".chart-tag-y");
  const tagVolume = layer.querySelector(".chart-tag-volume");
  const tagMacd = layer.querySelector(".chart-tag-macd");
  const tagDmi = layer.querySelector(".chart-tag-dmi");
  const tagMfi = layer.querySelector(".chart-tag-mfi");
  let model = initial.chart;
  let scenariosVisible = initial.scenariosVisible !== false;
  let onScenarioHover = initial.onScenarioHover;
  let onScenarioLeave = initial.onScenarioLeave;
  let hoveredScenario = null;
  let activeIndex = null;
  const spotCoverRect = layer.querySelector(".chan-spot-cover-rect");
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
  const indicatorReadout = document.createElement("div");
  indicatorReadout.className = "chart-tooltip-sub chan-indicator-readout";
  indicatorReadout.hidden = true;
  const structureDetail = document.createElement("section");
  structureDetail.className = "chan-structure-tooltip";
  structureDetail.hidden = true;
  const structureTitle = document.createElement("strong");
  const structureBody = document.createElement("span");
  structureDetail.append(structureTitle, structureBody);
  tooltip.replaceChildren(title, subtitle);
  const values = {};
  [["open", "开盘"], ["close", "收盘"], ["high", "最高"], ["low", "最低"]].forEach(([key, label]) => {
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
  tooltip.append(indicatorReadout);
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
  const show = (index, localY, localXOverride = null, pendingDate = "") => {
    const candle = pendingDate ? {
      key: `pending-${pendingDate}`, date: pendingDate, displayDate: pendingDate,
      x: localXOverride, pending: true, prices: {}, volume: null,
    } : model?.candles?.[index];
    if (!candle || !target.offsetWidth || !target.offsetHeight) { hide(); return; }
    activeIndex = index;
    const candleIdentity = candle.pending ? candle.key : candle;
    if (shownHeaderCandle !== candleIdentity) {
      drawChanIndicatorHeaders(target, model, candle);
      shownHeaderCandle = candleIdentity;
    }
    const localX = Number.isFinite(localXOverride) ? localXOverride : candle.x;
    const crosshairX = candle.pending ? localX : candle.x;
    const crosshairBottom = Number.isFinite(model.crosshairBottom) ? model.crosshairBottom : model.plotBottom;
    const inPricePane = localY >= model.plotTop && localY <= model.plotBottom;
    const inVolumePane = Number.isFinite(model.volumeTop) && Number.isFinite(model.volumeBottom)
      && localY >= model.volumeTop && localY <= model.volumeBottom;
    const inMacdPane = Number.isFinite(model.macdTop) && Number.isFinite(model.macdBottom)
      && localY >= model.macdTop && localY <= model.macdBottom;
    const inDmiPane = Number.isFinite(model.dmiTop) && Number.isFinite(model.dmiBottom)
      && localY >= model.dmiTop && localY <= model.dmiBottom;
    const inMfiPane = Number.isFinite(model.mfiTop) && Number.isFinite(model.mfiBottom)
      && localY >= model.mfiTop && localY <= model.mfiBottom;
    const plotHeight = model.plotBottom - model.plotTop;
    const priceRange = model.maximum - model.minimum;
    const cursorY = Math.max(model.plotTop, Math.min(model.plotBottom, localY));
    const price = model.maximum - (cursorY - model.plotTop) / plotHeight * priceRange;
    setLine(vertical, crosshairX, model.plotTop, crosshairX, crosshairBottom);
    // The Chan chart reserves its right side for scenario cards; keep the
    // historical crosshair inside the candle plot so it cannot strike text.
    if (inPricePane) {
      setLine(horizontal, model.plotLeft, cursorY, candle.pending ? model.plotRight : model.historyRight ?? model.plotRight, cursorY);
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
    if (horizontalMfi) {
      if (inMfiPane) {
        setLine(horizontalMfi, model.plotLeft, localY, model.plotRight, localY);
        horizontalMfi.removeAttribute("visibility");
      } else horizontalMfi.setAttribute("visibility", "hidden");
    }
    const displayDate = candle.displayDate || candle.date;
    const axisDate = displayDate.includes(" ") ? displayDate.slice(5) : displayDate;
    const dateWidth = Math.max(84, axisDate.length * 7 + 12);
    setTag(tagX, axisDate, Math.max(2, Math.min(model.width - dateWidth - 2, crosshairX - dateWidth / 2)), model.height - 22, dateWidth);
    const priceLabel = formatMoney(price), priceWidth = Math.min(model.plotLeft - 4, Math.max(42, priceLabel.length * 6 + 10));
    // Preserve the current-price badge when the cursor reaches the same price.
    const priceX = Math.max(2, model.plotLeft - priceWidth - 4);
    setTag(tagY, priceLabel, priceX, cursorY - 9, priceWidth);
    if (!inPricePane) tagY.setAttribute("visibility", "hidden");
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
    const mfiAtCursor = inMfiPane
      ? 100 * (model.mfiBottom - localY) / Math.max(1, model.mfiBottom - model.mfiTop) : null;
    setPaneTag(tagVolume, inVolumePane, volumeAtCursor == null ? "" : formatVolume(Math.max(0, volumeAtCursor)), localY);
    setPaneTag(tagMacd, inMacdPane, macdAtCursor == null ? "" : formatIndicator(macdAtCursor), localY);
    setPaneTag(tagDmi, inDmiPane, dmiAtCursor == null ? "" : formatIndicator(dmiAtCursor), localY);
    setPaneTag(tagMfi, inMfiPane, mfiAtCursor == null ? "" : formatIndicator(mfiAtCursor), localY);
    if (candle.pending ? shownCandle?.key !== candle.key : shownCandle !== candle) {
      title.textContent = displayDate;
      subtitle.textContent = `${model.periodLabel} · ${candle.pending ? "待开盘 K 线" : candle.isForming ? "形成中 K 线" : "已收盘 K 线"}`;
      const directionClass = candle.pending ? "chan-ohlc-flat" : candle.prices.close > candle.prices.open ? "chan-ohlc-up"
        : candle.prices.close < candle.prices.open ? "chan-ohlc-down" : "chan-ohlc-flat";
      Object.keys(values).forEach((key) => {
        values[key].textContent = candle.pending ? "-" : formatMoney(candle.prices[key]);
        values[key].className = `chan-ohlc-value ${directionClass}`;
      });
      const change = candle.closeChangePercent;
      changeValue.textContent = candle.pending ? "-" : Number.isFinite(change)
        ? `${change > 0 ? "+" : ""}${change.toFixed(2)}%` : "未知";
      changeValue.className = `chan-ohlc-value ${candle.pending || !Number.isFinite(change) ? "chan-ohlc-flat" : change > 0 ? "chan-ohlc-up" : change < 0 ? "chan-ohlc-down" : "chan-ohlc-flat"}`;
      volumeValue.textContent = candle.pending ? "-" : formatVolume(candle.volume);
      const i = candle.index;
      const vwap = model.vwapLines?.[0]?.[i]?.value;
      const anchored = model.vwapLines?.[1]?.[i]?.value;
      const bandwidth = model.bollinger?.series?.[i]?.width;
      const rsi = model.rsi?.series?.[i]?.value;
      const rs = model.rs?.series?.[i]?.value;
      const indicators = [
        Number.isFinite(vwap) && `VWAP ${formatMoney(vwap)}`,
        Number.isFinite(anchored) && `锚定 VWAP ${formatMoney(anchored)}`,
        Number.isFinite(bandwidth) && `带宽 ${(bandwidth * 100).toFixed(2)}%`,
        Number.isFinite(rsi) && `RSI ${rsi.toFixed(1)}`,
        Number.isFinite(rs) && `RS ${rs.toFixed(2)}`,
      ].filter(Boolean);
      indicatorReadout.textContent = indicators.join(" · ");
      indicatorReadout.hidden = candle.pending || indicators.length === 0;
      shownCandle = candle;
    }
    const distanceToSegment = (item) => {
      const dx = item.x2 - item.x1, dy = item.y2 - item.y1;
      const lengthSquared = dx * dx + dy * dy;
      const ratio = lengthSquared ? Math.max(0, Math.min(1, ((localX - item.x1) * dx + (localY - item.y1) * dy) / lengthSquared)) : 0;
      return Math.hypot(localX - (item.x1 + ratio * dx), localY - (item.y1 + ratio * dy));
    };
    const signalHit = candle.pending ? null : (model.signals || []).map((signal) => {
      const marker = signal.renderedMarker;
      const markerDistance = marker ? (Math.hypot((localX - marker.x) / marker.hitX, (localY - marker.y) / marker.hitY) <= 1 ? 0 : Infinity)
        : Math.hypot(localX - signal.x, localY - (Number.isFinite(signal.markerY) ? signal.markerY : signal.y));
      const halfLabel = (signal.hitLabelWidth || 48) / 2;
      const labelX = Number.isFinite(signal.labelX) ? signal.labelX : signal.x;
      const label = signal.renderedLabel;
      const onLabel = label ? Math.abs(localY - label.y) <= label.height / 2 && Math.abs(localX - label.x) <= label.width / 2
        : localY >= signal.labelY - 12 && localY <= signal.labelY + 4 && Math.abs(localX - labelX) <= halfLabel;
      const labelDistance = signal.labelVisible !== false && onLabel ? 0 : Infinity;
      return { signal, distance: Math.min(markerDistance, labelDistance) };
    }).filter(({ signal, distance }) => model.layers?.signals === true && distance <= 12)
      .sort((left, right) => left.distance - right.distance)[0]?.signal;
    const afSignalHit = candle.pending ? null : (model.afSignals || []).map((signal) => {
      const markerDistance = Math.hypot(localX - signal.x, localY - signal.markerY);
      const halfLabel = (signal.hitLabelWidth || 48) / 2;
      const halfLabelHeight = (signal.labelHeight || 18) / 2;
      const labelDistance = localY >= signal.labelY - halfLabelHeight && localY <= signal.labelY + halfLabelHeight && Math.abs(localX - signal.labelX) <= halfLabel ? 0 : Infinity;
      return { signal, distance: Math.min(markerDistance, labelDistance) };
    }).filter(({ distance }) => model.layers?.afStochZ === true && distance <= 14)
      .sort((left, right) => left.distance - right.distance)[0]?.signal;
    if (signalHit) {
      const details = chanStructureTooltipDetails(signalHit);
      structureTitle.textContent = details.title;
      structureBody.textContent = details.body;
      structureDetail.hidden = false;
    } else if (afSignalHit) {
      structureTitle.textContent = afSignalHit.type || afSignalHit.label?.replace("\n", " · ") || "自适应随机Z信号";
      structureBody.textContent = `${afSignalHit.date} · 确认于 ${afSignalHit.confirmedAt || "未知"} · Z ${formatIndicator(afSignalHit.z)}`;
      structureDetail.hidden = false;
    }
    const priceReferences = [...(model.structureSupports || []), ...(model.keyLevels || []).filter((item) => item.tone === "resistance")];
    const supportHit = model.layers?.supportResistance === false ? null : priceReferences.find((item) => {
      const onLine = localX >= item.x1 && localX <= (item.lineEndX ?? item.x2) && Math.abs(localY - item.y) <= (item.lineHitPadding || 7);
      const onLabel = localX >= item.labelX - (item.labelWidth || 100) && localX <= item.labelX
        && Math.abs(localY - item.labelY) <= (item.labelHeight || 18) / 2;
      return onLine || onLabel;
    });
    // 信号统一命中真实绘制标记，避免隐藏预警及标记下方价格点出现幽灵命中。
    const structure = candle.pending ? null : (model.structures || []).filter((item) => !item.signalTarget).map((item) => {
      const inArea = item.area && localX >= Math.min(item.x1, item.x2) && localX <= Math.max(item.x1, item.x2)
        && localY >= Math.min(item.y1, item.y2) && localY <= Math.max(item.y1, item.y2);
      return { item, distance: item.area ? (inArea ? 5 : Infinity) : distanceToSegment(item) };
    })
      .filter(({ item, distance }) => distance <= (item.area ? 0 : 8))
      .sort((left, right) => left.distance - right.distance)[0]?.item;
    if (!signalHit && !afSignalHit && supportHit) {
      if (supportHit.mergedLevels?.length > 1) {
        const status = supportHit.tone === "resistance" ? "压力区" : supportHit.status === "broken" ? "失守区"
          : supportHit.status === "pending-break" ? "跌破待确认区" : "支撑区";
        structureTitle.textContent = `${status} · ${supportHit.zoneLabel}`;
        // 每个成员单独列出证据，避免相邻区域合并后虚增反弹次数或覆盖确认时间。
        structureBody.textContent = `包含 ${supportHit.mergedLevels.length} 个相近区域：` + supportHit.mergedLevels.map((level) => {
          const state = level.status === "pending-break" ? ` · 首次有效收盘跌破 ${level.pendingBreakAt} · 收盘 ${level.lastCloseLabel} · 有效跌破阈值 ${level.breakThresholdLabel} · 尚需下一根完成收盘确认`
            : level.status === "broken" ? ` · 失守 ${level.brokenAt}` : ` · 截至 ${level.lastCloseDate || "当前"} 未确认失效`;
          const tests = level.testCount ? ` · ${level.tone === "resistance" ? "独立受阻" : "有效反弹"} ${level.testCount} 次 · 最近测试 ${level.lastTestDate}` : "";
          const historical = level.historicalTestCount ? ` · 历史受阻 ${level.historicalTestCount} 次 · 旧压力突破 ${level.historicalBrokenAt}` : "";
          return `${level.zoneLabel || level.priceLabel || formatMoney(level.price)} · ${level.sources || level.label} · 确认 ${level.confirmedAt || level.date}${state}${tests}${historical}`;
        }).join("；");
        if (supportHit.resistanceZone) structureBody.textContent += ` · 同区反弹受阻 ${supportHit.resistanceZone.testCount} 次`;
      } else if (supportHit.tone === "resistance") {
        const historicalRetest = supportHit.kind === "historical-retest";
        structureTitle.textContent = historicalRetest ? `历史压力回测 · ${supportHit.zoneLabel}`
          : supportHit.testCount ? `反复受阻压力 · ${supportHit.zoneLabel}` : `结构前高压力 · ${supportHit.priceLabel}`;
        structureBody.textContent = historicalRetest
          ? `${supportHit.sources} · 历史受阻 ${supportHit.historicalTestCount} 次 · 旧压力突破 ${supportHit.historicalBrokenAt} · 本轮回测 ${supportHit.testCount} 次 · 最近测试 ${supportHit.lastTestDate} · 确认 ${supportHit.confirmedAt}`
          : supportHit.testCount ? `${supportHit.sources} · 独立受阻 ${supportHit.testCount} 次 · 最近测试 ${supportHit.lastTestDate} · 确认 ${supportHit.confirmedAt}${supportHit.reestablished ? " · 突破后重新确认" : ""}`
            : `${supportHit.date} · ${supportHit.label}`;
      } else {
        structureTitle.textContent = `${supportHit.statusLabel} · ${supportHit.zoneLabel}`;
        const supportState = supportHit.status === "broken" ? " · 失守 " + supportHit.brokenAt
          : supportHit.status === "pending-break" ? ` · 首次有效收盘跌破 ${supportHit.pendingBreakAt} · 收盘 ${supportHit.lastCloseLabel} · 有效跌破阈值 ${supportHit.breakThresholdLabel} · 尚需下一根完成收盘确认`
            : " · 截至 " + supportHit.lastCloseDate + " 未确认收盘失守";
        structureBody.textContent = `${supportHit.sources} · 起点 ${supportHit.date} · 确认 ${supportHit.confirmedAt}${supportState}`;
        if (supportHit.resistanceZone) {
          const pressure = supportHit.resistanceZone;
          structureBody.textContent += pressure.kind === "historical-retest"
            ? ` · 同区历史压力回测 ${pressure.testCount} 次（历史受阻 ${pressure.historicalTestCount} 次）`
            : ` · 同区反弹受阻 ${pressure.testCount} 次`;
        }
        if (supportHit.testCount) structureBody.textContent += ` · 有效反弹 ${supportHit.testCount} 次 · 最近测试 ${supportHit.lastTestDate} · 平均反弹 ${supportHit.reactionAtr.toFixed(2)} ATR${supportHit.reestablished ? " · 失守后重新确认" : ""}`;
        if (supportHit.hasHistoricalStructure) structureBody.textContent += ` · 历史结构确认 ${supportHit.historicalDate}${supportHit.kind === "historical" ? " · 长期参考，未近期回踩不等于失守" : ""}`;
      }
      structureDetail.hidden = false;
    } else if (!signalHit && !afSignalHit && structure) {
      const detail = chanStructureTooltipDetails(structure);
      structureTitle.textContent = detail.title;
      structureBody.textContent = detail.body;
      structureDetail.hidden = false;
    } else if (!signalHit && !afSignalHit) {
      structureDetail.hidden = true;
      structureTitle.textContent = ""; structureBody.textContent = "";
    }
    layer.setAttribute("visibility", "visible");
    // 十字线出现时用 SVG 矩形遮住左轴现价徽章，避免遮挡 K 线价格；
    // cover rect 在 tagY 之前渲染，tagY 会叠在上方保持可见。
    if (spotCoverRect && model.spot) {
      const spotHeight = model.spot.height || 22;
      spotCoverRect.setAttribute("x", 0);
      spotCoverRect.setAttribute("y", model.spot.y - spotHeight / 2 - 2);
      spotCoverRect.setAttribute("width", Math.max(0, model.plotLeft - 2));
      spotCoverRect.setAttribute("height", spotHeight + 4);
      spotCoverRect.removeAttribute("visibility");
    }
    tooltip.hidden = false;
    const matrix = svg.getScreenCTM();
    if (!matrix) { hide(); return; }
    const point = svg.createSVGPoint();
    point.x = crosshairX; point.y = cursorY;
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
    if (point.x < model.plotLeft || point.x > model.plotRight || point.y < model.plotTop || point.y > (model.crosshairBottom ?? model.plotBottom)) { hide(); return; }
    lastPointer = event;
    const visibleBars = Math.max(1, Number(model.visibleBars) || model.candles.length);
    const historyRight = model.historyRight ?? model.plotRight;
    const slot = (historyRight - model.plotLeft) / visibleBars;
    if (point.x > historyRight) {
      // 右侧预留区用于条件情景路径；它没有对应的 K 线日期。
      // 真正的未来槽会在 historyRight 以内按时间轴索引命中。
      hide();
      return;
    }
    const index = Math.floor((point.x - model.plotLeft) / slot);
    if (index < model.candles.length) {
      show(index, point.y, point.x);
      return;
    }
    const pendingDate = model.pendingOpenDates?.[index - model.candles.length];
    if (!pendingDate) { hide(); return; }
    show(index, point.y, point.x, pendingDate);
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
    const current = activeIndex == null ? last : Math.max(0, Math.min(last, activeIndex));
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

// 条件情景开关使用 Element UI 的帮助图标字体；字体载入后重绘图表。
let elementIconFontReady = false;
let elementIconFontAttempted = false;
function ensureElementIconFont(render) {
  if (elementIconFontReady || elementIconFontAttempted) return;
  elementIconFontAttempted = true;
  if (typeof document === "undefined" || !document.fonts?.load) return;
  document.fonts.load("11px element-icons").then(() => {
    elementIconFontReady = document.fonts.check("11px element-icons");
    render();
  }).catch(() => { /* 字体载入失败时使用普通字符兜底。 */ });
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
  if (value == null || value === "") return "--";
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
  if (candle?.pending) {
    return {
      volume: "成交量  -",
      macd: "MACD  -", macdCompact: "MACD -", macdInterpretation: "", macdCompactInterpretation: "",
      dmi: "DMI  -", dmiCompact: "DMI -", dmiStatus: "", dmiInterpretation: "",
      mfi: "MFI  -", mfiCompact: "MFI -", mfiStatus: "", mfiInterpretation: "",
    };
  }
  const volume = candle && Number.isFinite(Number(candle.volume)) ? formatVolume(candle.volume) : "--";
  const macd = candle?.macd;
  const dmi = candle?.dmi;
  const mfi = candle?.mfi;
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
    mfi: Number.isFinite(mfi?.value)
      ? `MFI ${formatIndicator(mfi.value)} · 均线 ${formatIndicator(mfi.average)} · 标普 ${formatIndicator(mfi.benchmark)}`
      : "MFI（14）预热中",
    mfiCompact: Number.isFinite(mfi?.value)
      ? `MFI ${compactIndicatorValue(mfi.value)} · 均线 ${compactIndicatorValue(mfi.average)} · 标普 ${compactIndicatorValue(mfi.benchmark)}`
      : "MFI 预热中",
    mfiStatus: !Number.isFinite(mfi?.value) ? "预热中"
      : mfi.value >= 80 ? "超买区"
        : mfi.value <= 20 ? "超卖区"
          : Number.isFinite(mfi.benchmark) ? (mfi.value > mfi.benchmark ? "个股强于标普" : mfi.value < mfi.benchmark ? "个股弱于标普" : "与标普接近") : "标普数据暂缺",
    mfiInterpretation: !Number.isFinite(mfi?.value) ? "MFI指标预热中"
      : mfi.value >= 80 ? "MFI高于80，资金流处于超买区"
        : mfi.value <= 20 ? "MFI低于20，资金流处于超卖区"
          : Number.isFinite(mfi.benchmark) ? (mfi.value > mfi.benchmark ? "个股资金强于标普" : "个股资金弱于标普") : "等待标普MFI对比",
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
    || (![model.volumeTop, model.macdTop, model.dmiTop, model.mfiTop].some(Number.isFinite))) return;
  const svg = target.querySelector("svg");
  const chartWidth = typeof svg?.getBoundingClientRect === "function" ? svg.getBoundingClientRect().width || 0 : 0;
  const leftBleed = Number(canvas.dataset?.leftBleed) || 0;
  const pixelScale = canvas.width / (chartWidth + leftBleed);
  context.setTransform(pixelScale * chartWidth / model.width, 0, 0, canvas.height / model.height,
    pixelScale * leftBleed, 0);
  const color = chanThemeColor(target, "--muted", "#81909c");
  const width = Math.max(1, model.plotRight - model.plotLeft + 4);
  context.font = indicatorHeaderFont(model);
  context.fillStyle = color;
  context.textAlign = "left";
  context.textBaseline = "alphabetic";
  const header = indicatorHeaderText(candle);
  if (Number.isFinite(model.volumeTop)) {
    context.clearRect(model.plotLeft - 2, model.volumeTop - 18, width, 16);
    context.fillText(header.volume, model.plotLeft, model.volumeTop - 6);
  }
  const bullishColor = chanThemeColor(target, "--up", "#19bd83");
  const bearishColor = chanThemeColor(target, "--down", "#f04d68");
  const macdColor = candle?.macd?.hist > 0 ? bullishColor : candle?.macd?.hist < 0 ? bearishColor : color;
  if (Number.isFinite(model.macdTop)) {
    context.clearRect(model.plotLeft - 2, model.macdTop - 18, width, 16);
    drawInlineIndicatorHeader(context, model, model.plotLeft, model.macdTop - 6, width,
      header.macd, header.macdInterpretation, header.macdCompact, header.macdCompactInterpretation, color, macdColor);
  }
  if (!Number.isFinite(model.dmiTop)) {
    if (Number.isFinite(model.mfiTop)) {
      context.clearRect(model.plotLeft - 2, model.mfiTop - 18, width, 16);
      const value = Number(candle?.mfi?.value);
      const mfiColor = Number.isFinite(value) ? (value >= 80 ? bearishColor : value <= 20 ? bullishColor : color) : color;
      drawInlineIndicatorHeader(context, model, model.plotLeft, model.mfiTop - 6, width,
        header.mfi, header.mfiInterpretation, header.mfiCompact, header.mfiStatus, color, mfiColor);
    }
    return;
  }
  context.clearRect(model.plotLeft - 2, model.dmiTop - 18, width, 16);
  // 美股图表使用绿色表示上涨、红色表示下跌。
  const dmiDifference = Number(candle?.dmi?.difference);
  const dmiColor = !Number.isFinite(dmiDifference) || Math.abs(dmiDifference) < 0.01
    ? color : dmiDifference > 0 ? bullishColor : bearishColor;
  drawInlineIndicatorHeader(context, model, model.plotLeft, model.dmiTop - 6, width,
    header.dmi, header.dmiInterpretation, header.dmiCompact, header.dmiStatus, color, dmiColor);
  if (Number.isFinite(model.mfiTop)) {
    context.clearRect(model.plotLeft - 2, model.mfiTop - 18, width, 16);
    const value = Number(candle?.mfi?.value);
    const mfiColor = Number.isFinite(value) ? (value >= 80 ? bearishColor : value <= 20 ? bullishColor : color) : color;
    drawInlineIndicatorHeader(context, model, model.plotLeft, model.mfiTop - 6, width,
      header.mfi, header.mfiInterpretation, header.mfiCompact, header.mfiStatus, color, mfiColor);
  }
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
  if (!canvas.dataset) canvas.dataset = {};
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
    supportPending: chanThemeColor(target, "--chan-support-pending", chanThemeColor(target, "--amber", "#e7b45c")),
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
  if (model.vwapLines) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    model.vwapLines.forEach((series, index) => {
      if (!series) return;
      const color = index === 0 ? colors.blue : colors.amber;
      let previous = null;
      for (const point of series) {
        if (point && previous?.group === point.group) {
          line(previous.x, previous.y, point.x, point.y, color, point.live ? [2, 2] : index === 0 ? [] : [5, 3], 1.25);
        } else if (point) {
          context.fillStyle = color;
          context.fillRect(point.x - 1.5, point.y - 1.5, 3, 3);
        }
        previous = point;
      }
    });
    context.restore();
  }
  if (model.layers?.wyckoff === true && model.wyckoffRange) {
    const range = model.wyckoffRange;
    const phaseColors = {
      B: { accumulation: "#1ecdcf", distribution: "#aa5ada" },
      C: { accumulation: "#afcd2d", distribution: "#eb6937" },
      D: { accumulation: "#78d24b", distribution: "#d74691" },
      E: { accumulation: "#1ea591", distribution: "#b9374b" },
    };
    const color = phaseColors[range.phase]?.[range.bias]
      || (range.bias === "accumulation" ? colors.up : colors.down);
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    context.fillStyle = color;
    context.globalAlpha = 0.055;
    context.fillRect(range.x, range.y, range.width, range.height);
    context.globalAlpha = 0.72;
    context.strokeStyle = color;
    context.lineWidth = model.mobileChart ? 1.4 : 1.1;
    context.setLineDash([5, 3]);
    context.strokeRect(range.x, range.y, range.width, range.height);
    context.setLineDash([]);
    context.globalAlpha = 1;
    const phaseLabel = `${range.bias === "accumulation" ? "吸筹" : "派发"} · 阶段 ${range.phase}`;
    context.font = `650 ${model.mobileChart ? 10 : 9}px Inter, ui-sans-serif, system-ui, sans-serif`;
    context.textAlign = "left";
    context.textBaseline = "alphabetic";
    const labelWidth = context.measureText(phaseLabel).width + 10;
    const labelHeight = model.mobileChart ? 18 : 16;
    const labelX = Math.max(model.plotLeft, Math.min(model.historyRight - labelWidth, range.x + 3));
    const labelY = Math.min(model.plotBottom - 2, Math.max(model.plotTop + labelHeight, range.y + labelHeight + 1));
    context.globalAlpha = 0.92;
    context.fillStyle = colors.panel;
    context.fillRect(labelX, labelY - labelHeight + 2, labelWidth, labelHeight);
    context.globalAlpha = 1;
    context.fillStyle = color;
    context.fillText(phaseLabel, labelX + 5, labelY - 3);
    context.restore();
  }
  if (model.bollinger) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    for (const key of ["upperY", "middleY", "lowerY"]) {
      let previous = null;
      for (const point of model.bollinger.series) {
        if (point && previous) {
          context.globalAlpha = key === "middleY" ? 0.65 : 0.82;
          line(previous.x, previous[key], point.x, point[key], colors.net, point.live || key === "middleY" ? [4, 3] : [], 1);
        }
        previous = point;
      }
    }
    context.globalAlpha = 1;
    let lastMarkerX = -Infinity;
    for (const event of model.bollinger.events) {
      if (event.x - lastMarkerX < 12 && event !== model.bollinger.events.at(-1)) continue;
      const upward = event.side === "up";
      const color = upward ? colors.up : colors.down;
      const markerY = Math.max(model.plotTop + 5, Math.min(model.plotBottom - 5, event.y + (upward ? -7 : 7)));
      context.beginPath();
      context.moveTo(event.x, markerY + (upward ? -4 : 4));
      context.lineTo(event.x - 3, markerY + (upward ? 2 : -2));
      context.lineTo(event.x + 3, markerY + (upward ? 2 : -2));
      context.closePath();
      if (event.status === "watch") { context.strokeStyle = color; context.stroke(); }
      else { context.fillStyle = color; context.fill(); }
      lastMarkerX = event.x;
    }
    context.restore();
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
  // 扫流动性反转的 FVG 与入场窗口使用独立覆盖层；所有绘制都限制在历史价格区，
  // 不参与坐标范围计算，也不会改变 K 线高度。
  if (model.layers?.sweepFvg === true && model.sweepFvg) {
    const sweepFvg = model.sweepFvg;
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    for (const windowState of sweepFvg.windows || []) {
      const bullish = windowState.direction === "bullish";
      const color = bullish ? colors.up : colors.down;
      context.globalAlpha = windowState.active ? 0.18 : 0.08;
      context.fillStyle = color;
      context.fillRect(windowState.fvgX, windowState.fvgY, windowState.fvgWidth, windowState.fvgHeight);
      context.globalAlpha = windowState.active ? 0.86 : 0.46;
      context.strokeStyle = color;
      context.lineWidth = model.mobileChart ? 1.2 : 1;
      context.setLineDash(windowState.active ? [4, 3] : [2, 3]);
      context.strokeRect(windowState.fvgX, windowState.fvgY, windowState.fvgWidth, windowState.fvgHeight);
      context.globalAlpha = windowState.active ? 0.28 : 0.13;
      context.fillStyle = color;
      context.fillRect(windowState.x, windowState.y, windowState.width, windowState.height);
      context.globalAlpha = windowState.active ? 0.7 : 0.34;
      context.strokeStyle = color;
      context.setLineDash([3, 3]);
      context.strokeRect(windowState.x, windowState.y, windowState.width, windowState.height);
      context.setLineDash([]);
      context.globalAlpha = 1;
    }
    const sweeps = sweepFvg.sweeps || [];
    for (const sweep of sweeps) {
      const color = sweep.direction === "bullish" ? colors.up : colors.down;
      const markerRadius = model.mobileChart ? 5 : 4.5;
      context.globalAlpha = 0.52;
      line(sweep.x1, sweep.y1, sweep.x2, sweep.y2, color, [3, 3], model.mobileChart ? 1.2 : 1);
      context.globalAlpha = 0.94;
      context.beginPath();
      context.arc(sweep.x, sweep.y, markerRadius, 0, Math.PI * 2);
      context.fillStyle = colors.panel; context.fill();
      context.strokeStyle = color; context.lineWidth = 1.5; context.stroke();
      context.globalAlpha = 1;
    }
    for (const structureBreak of sweepFvg.breaks || []) {
      const color = structureBreak.direction === "bullish" ? colors.up : colors.down;
      line(structureBreak.x1, structureBreak.y, structureBreak.x2, structureBreak.y, color, [6, 3], model.mobileChart ? 1.3 : 1);
    }
    for (const event of sweepFvg.events || []) {
      const color = event.side === "buy" ? colors.up : colors.down;
      const radius = model.mobileChart ? 5 : 4.5;
      const up = event.side === "buy";
      context.beginPath();
      context.moveTo(event.x, event.y + (up ? radius + 1 : -radius - 1));
      context.lineTo(event.x - radius, event.y + (up ? -radius : radius));
      context.lineTo(event.x + radius, event.y + (up ? -radius : radius));
      context.closePath();
      context.fillStyle = color; context.fill();
      context.strokeStyle = colors.panel; context.lineWidth = 1.4; context.stroke();
      const fontSize = model.mobileChart ? 11 : 10;
      context.font = `700 ${fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
      context.textAlign = "center"; context.textBaseline = "alphabetic";
      const labelY = Math.max(model.plotTop + fontSize + 4, Math.min(model.plotBottom - 3, event.y + (up ? 24 : -14)));
      const labelWidth = context.measureText(event.label).width + 10;
      const labelCenterX = Math.max(model.plotLeft + labelWidth / 2 + 2,
        Math.min(model.historyRight - labelWidth / 2 - 2, event.x));
      context.lineWidth = 3;
      context.lineJoin = "round";
      context.strokeStyle = colors.panel;
      context.strokeText(event.label, labelCenterX, labelY);
      context.fillStyle = color; context.fillText(event.label, labelCenterX, labelY);
    }
    context.restore();
  }
  if (model.layers?.turtle === true && model.turtleChannels?.length) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    const drawStepChannel = (field, dash, alpha) => {
      context.beginPath();
      context.setLineDash(dash);
      context.strokeStyle = colors.blue;
      context.lineWidth = model.mobileChart ? 1.7 : 1.2;
      context.globalAlpha = alpha;
      let active = false, previousY = NaN;
      for (const point of model.turtleChannels) {
        const value = point?.[field];
        if (!point || !Number.isFinite(value)) {
          if (active) context.stroke();
          context.beginPath();
          active = false;
          previousY = NaN;
          continue;
        }
        if (!active) {
          context.moveTo(point.x, value);
          active = true;
        } else {
          context.lineTo(point.x, previousY);
          context.lineTo(point.x, value);
        }
        previousY = value;
      }
      if (active) context.stroke();
      context.setLineDash([]);
      context.globalAlpha = 1;
    };
    drawStepChannel("entryUpperY", [], 0.9);
    drawStepChannel("entryLowerY", [], 0.9);
    drawStepChannel("exitUpperY", [4, 3], 0.48);
    drawStepChannel("exitLowerY", [4, 3], 0.48);
    context.restore();
    for (const event of model.turtleEvents || []) {
      const color = event.kind === "long-entry" || event.kind === "short-exit" ? colors.up : colors.down;
      const radius = Number.isFinite(event.markerRadius) ? event.markerRadius : (model.mobileChart ? 5 : 4);
      context.beginPath();
      context.moveTo(event.x, event.y + (event.upMarker ? -radius : radius));
      context.lineTo(event.x - radius, event.y + (event.upMarker ? radius : -radius));
      context.lineTo(event.x + radius, event.y + (event.upMarker ? radius : -radius));
      context.closePath();
      context.fillStyle = color;
      context.fill();
      context.font = `650 ${event.fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
      context.textAlign = "center";
      context.textBaseline = "alphabetic";
      context.lineWidth = 3;
      context.lineJoin = "round";
      context.strokeStyle = colors.panel;
      context.strokeText(event.label, event.x, event.labelY);
      context.fillStyle = color;
      context.fillText(event.label, event.x, event.labelY);
    }
  }
  if (model.layers?.granville === true) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    context.beginPath();
    context.setLineDash([]);
    context.strokeStyle = colors.amber;
    context.lineWidth = model.mobileChart ? 2 : 1.5;
    context.globalAlpha = 0.9;
    let active = false;
    for (const point of model.granvilleAverage || []) {
      if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y)) {
        active = false;
        continue;
      }
      if (active) context.lineTo(point.x, point.y);
      else context.moveTo(point.x, point.y);
      active = true;
    }
    context.stroke();
    context.globalAlpha = 1;

    for (const event of model.granvilleEvents || []) {
      const color = event.side === "buy" ? colors.up : colors.down;
      const radius = Number.isFinite(event.markerRadius) ? event.markerRadius : (model.mobileChart ? 5 : 4);
      context.beginPath();
      context.moveTo(event.x, event.y + (event.upMarker ? -radius : radius));
      context.lineTo(event.x - radius, event.y + (event.upMarker ? radius : -radius));
      context.lineTo(event.x + radius, event.y + (event.upMarker ? radius : -radius));
      context.closePath();
      context.fillStyle = color;
      context.fill();
      if (event.labelConnector && event.labelVisible !== false) {
        const labelBelowMarker = event.labelY > event.y;
        const labelEdgeY = labelBelowMarker ? event.labelY - event.fontSize - 2 : event.labelY + 2;
        context.beginPath();
        context.moveTo(event.x, event.y + (labelBelowMarker ? radius + 1 : -radius - 1));
        context.lineTo(event.x, labelEdgeY);
        context.strokeStyle = color;
        context.lineWidth = 1;
        context.globalAlpha = 0.62;
        context.stroke();
        context.globalAlpha = 1;
      }
      if (event.labelVisible !== false) {
        const labelHalfWidth = Array.from(event.label).length * event.fontSize / 2 + 3;
        const labelTop = event.labelY - event.fontSize - 2;
        context.globalAlpha = 0.96;
        context.fillStyle = color;
        context.fillRect(event.x - labelHalfWidth, labelTop, labelHalfWidth * 2, event.fontSize + 4);
        context.globalAlpha = 1;
        context.font = `650 ${event.fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
        context.textAlign = "center";
        context.textBaseline = "alphabetic";
        context.fillStyle = colors.panel;
        context.fillText(event.label, event.x, event.labelY);
      }
    }
    context.restore();
  }
  if (model.layers?.macdSma200 === true) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    const fastAverage = model.macdSma200Averages?.fast || [];
    const trendAverage = model.macdSma200Averages?.trend || [];
    let fillSegment = [];
    const paintAverageFill = () => {
      if (fillSegment.length > 1) {
        context.beginPath();
        context.moveTo(fillSegment[0].fast.x, fillSegment[0].fast.y);
        for (const point of fillSegment) context.lineTo(point.fast.x, point.fast.y);
        for (let index = fillSegment.length - 1; index >= 0; index -= 1) {
          context.lineTo(fillSegment[index].trend.x, fillSegment[index].trend.y);
        }
        context.closePath();
        context.fillStyle = colors.axis;
        context.globalAlpha = 0.14;
        context.fill();
        context.globalAlpha = 1;
      }
      fillSegment = [];
    };
    for (let index = 0; index < Math.max(fastAverage.length, trendAverage.length); index += 1) {
      const fast = fastAverage[index], trend = trendAverage[index];
      if (fast && trend && Number.isFinite(fast.x) && Number.isFinite(fast.y)
        && Number.isFinite(trend.x) && Number.isFinite(trend.y)) {
        fillSegment.push({ fast, trend });
      } else paintAverageFill();
    }
    paintAverageFill();
    const averageTrendColor = (trend) => trend === "up" ? colors.up : trend === "down" ? colors.down : colors.blue;
    const drawTrendColoredAverage = (series, width) => {
      let active = false;
      let activeTrend = "neutral";
      let previous = null;
      for (const point of series) {
        if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y)) {
          if (active) context.stroke();
          active = false;
          previous = null;
          continue;
        }
        if (!active) {
          context.beginPath();
          context.strokeStyle = averageTrendColor(point.trend);
          context.lineWidth = width;
          context.moveTo(point.x, point.y);
          active = true;
        } else if (point.trend !== activeTrend) {
          context.stroke();
          context.beginPath();
          context.strokeStyle = averageTrendColor(point.trend);
          context.lineWidth = width;
          context.moveTo(previous.x, previous.y);
          context.lineTo(point.x, point.y);
        } else context.lineTo(point.x, point.y);
        activeTrend = point.trend || "neutral";
        previous = point;
      }
      if (active) context.stroke();
    };
    drawTrendColoredAverage(fastAverage, model.mobileChart ? 1.5 : 1);
    drawTrendColoredAverage(model.macdSma200Averages?.slow || [], model.mobileChart ? 2.3 : 2);
    for (let index = 1; index < trendAverage.length; index += 1) {
      const previous = trendAverage[index - 1], current = trendAverage[index];
      if (!previous || !current || !Number.isFinite(previous.x) || !Number.isFinite(previous.y)
        || !Number.isFinite(current.x) || !Number.isFinite(current.y)) continue;
      line(previous.x, previous.y, current.x, current.y, current.rising ? colors.up : colors.down, [], model.mobileChart ? 2 : 2.5);
    }
    for (const event of model.macdSma200Events || []) {
      const color = event.side === "buy" ? colors.up : colors.down;
      const radius = Number.isFinite(event.markerRadius) ? event.markerRadius : (model.mobileChart ? 5 : 4);
      context.beginPath();
      context.moveTo(event.x, event.y + (event.upMarker ? -radius : radius));
      context.lineTo(event.x - radius, event.y + (event.upMarker ? radius : -radius));
      context.lineTo(event.x + radius, event.y + (event.upMarker ? radius : -radius));
      context.closePath();
      context.fillStyle = color;
      context.fill();
      if (event.labelConnector && event.labelVisible !== false) {
        const labelBelowMarker = event.labelY > event.y;
        const labelEdgeY = labelBelowMarker ? event.labelY - event.fontSize - 2 : event.labelY + 2;
        context.beginPath();
        context.moveTo(event.x, event.y + (labelBelowMarker ? radius + 1 : -radius - 1));
        context.lineTo(event.x, labelEdgeY);
        context.strokeStyle = color;
        context.lineWidth = 1;
        context.globalAlpha = 0.62;
        context.stroke();
        context.globalAlpha = 1;
      }
      if (event.labelVisible !== false) {
        const labelHalfWidth = Array.from(event.label).length * event.fontSize / 2 + 3;
        const labelTop = event.labelY - event.fontSize - 2;
        context.globalAlpha = 0.96;
        context.fillStyle = color;
        context.fillRect(event.x - labelHalfWidth, labelTop, labelHalfWidth * 2, event.fontSize + 4);
        context.globalAlpha = 1;
        context.font = `650 ${event.fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
        context.textAlign = "center";
        context.textBaseline = "alphabetic";
        context.fillStyle = colors.panel;
        context.fillText(event.label, event.x, event.labelY);
      }
    }
    context.restore();
  }
  if (model.layers?.supertrend === true) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    const drawHighlight = (series, color) => {
      let segment = [];
      const fillSegment = () => {
        if (segment.length < 2) { segment = []; return; }
        context.beginPath();
        context.moveTo(segment[0].x, segment[0].y);
        for (let index = 1; index < segment.length; index += 1) context.lineTo(segment[index].x, segment[index].y);
        for (let index = segment.length - 1; index >= 0; index -= 1) context.lineTo(segment[index].x, segment[index].highlightY);
        context.closePath();
        context.fillStyle = color;
        context.globalAlpha = 0.08;
        context.fill();
        context.globalAlpha = 1;
        segment = [];
      };
      for (const point of series || []) {
        if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y) || !Number.isFinite(point.highlightY)) {
          fillSegment();
          continue;
        }
        segment.push(point);
      }
      fillSegment();
    };
    const drawTrend = (series, color) => {
      context.beginPath();
      context.strokeStyle = color;
      context.lineWidth = model.mobileChart ? 2.3 : 1.9;
      let active = false;
      for (const point of series || []) {
        if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y)) {
          active = false;
          continue;
        }
        if (active) context.lineTo(point.x, point.y);
        else context.moveTo(point.x, point.y);
        active = true;
      }
      context.stroke();
    };
    drawHighlight(model.supertrendLines?.up, colors.up);
    drawHighlight(model.supertrendLines?.down, colors.down);
    drawTrend(model.supertrendLines?.up, colors.up);
    drawTrend(model.supertrendLines?.down, colors.down);
    for (const event of model.supertrendEvents || []) {
      const color = event.side === "buy" ? colors.up : colors.down;
      context.beginPath();
      context.arc(event.x, event.y, model.mobileChart ? 3.8 : 3.2, 0, Math.PI * 2);
      context.fillStyle = color;
      context.fill();
      if (event.labelConnector && event.labelVisible !== false) {
        const labelBelowMarker = event.labelY > event.y;
        context.beginPath();
        context.moveTo(event.x, event.y + (labelBelowMarker ? 5 : -5));
        context.lineTo(event.x, labelBelowMarker ? event.labelY - event.fontSize - 2 : event.labelY + 2);
        context.strokeStyle = color;
        context.lineWidth = 1;
        context.globalAlpha = 0.62;
        context.stroke();
        context.globalAlpha = 1;
      }
      if (event.labelVisible === false) continue;
      context.font = `650 ${event.fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
      context.textAlign = "center";
      context.textBaseline = "alphabetic";
      context.lineWidth = 3;
      context.lineJoin = "round";
      context.strokeStyle = colors.panel;
      context.strokeText(event.label, event.x, event.labelY);
      context.fillStyle = color;
      context.fillText(event.label, event.x, event.labelY);
    }
    context.restore();
  }
  if (model.layers?.wyckoff === true) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    for (const event of model.wyckoffEvents || []) {
      const color = event.side === "buy" ? colors.up : colors.down;
      const radius = model.mobileChart ? 3.8 : 3.2;
      context.beginPath();
      context.arc(event.x, event.y, radius, 0, Math.PI * 2);
      context.fillStyle = color;
      context.fill();
      if (event.labelVisible === false) continue;
      if (event.labelConnector) {
        const labelBelowMarker = event.labelY > event.y;
        context.beginPath();
        context.moveTo(event.x, event.y + (labelBelowMarker ? radius + 1 : -radius - 1));
        context.lineTo(event.x, labelBelowMarker ? event.labelY - event.fontSize - 2 : event.labelY + 2);
        context.strokeStyle = color;
        context.lineWidth = 1;
        context.globalAlpha = 0.62;
        context.stroke();
        context.globalAlpha = 1;
      }
      context.font = `650 ${event.fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
      context.textAlign = "center";
      context.textBaseline = "alphabetic";
      context.lineWidth = 3;
      context.lineJoin = "round";
      context.strokeStyle = colors.panel;
      context.strokeText(event.label, event.x, event.labelY);
      context.fillStyle = color;
      context.fillText(event.label, event.x, event.labelY);
    }
    context.restore();
  }
  if (model.layers?.trendlines === true && model.trendlines) {
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.plotTop, model.historyRight - model.plotLeft, model.plotBottom - model.plotTop);
    context.clip();
    context.globalAlpha = 0.82;
    for (const trendline of model.trendlines || []) {
      const color = trendline.side === "support" ? colors.up : colors.down;
      const width = model.mobileChart ? 1.8 : 1.35;
      if (trendline.solid) line(trendline.solid.x1, trendline.solid.y1, trendline.solid.x2, trendline.solid.y2, color, [], width);
      if (trendline.extension) line(trendline.extension.x1, trendline.extension.y1, trendline.extension.x2, trendline.extension.y2, color, [4, 4], width);
      // 端点采用很小的圆点，强化结构锚点但不遮盖 K 线实体。
      for (const point of trendline.solid ? [
        { x: trendline.solid.x1, y: trendline.solid.y1 },
        { x: trendline.solid.x2, y: trendline.solid.y2 },
      ] : []) {
        context.beginPath(); context.fillStyle = color;
        context.arc(point.x, point.y, model.mobileChart ? 2.4 : 1.8, 0, Math.PI * 2); context.fill();
      }
    }
    context.restore();
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
    const labelHeight = marker.labelHeight || (model.mobileChart ? 18 : 15);
    if (marker.labelConnector) {
      const labelBelowMarker = marker.labelY > marker.y;
      const labelEdgeY = marker.labelY + (labelBelowMarker ? -labelHeight / 2 : labelHeight / 2);
      context.beginPath();
      context.moveTo(marker.x, marker.y + (labelBelowMarker ? 6 : -6));
      context.lineTo(marker.labelX, labelEdgeY);
      context.strokeStyle = markerColor; context.lineWidth = 1; context.globalAlpha = 0.62; context.stroke(); context.globalAlpha = 1;
    }
    context.font = model.mobileChart ? "550 12px Inter, ui-sans-serif, system-ui, sans-serif" : "9px Inter, ui-sans-serif, system-ui, sans-serif"; context.textAlign = "center"; context.textBaseline = "middle";
    context.lineWidth = 3; context.strokeStyle = colors.panel; context.strokeText(marker.label, marker.labelX, marker.labelY);
    context.fillStyle = markerColor; context.fillText(marker.label, marker.labelX, marker.labelY);
  }
  // 普通低点和中枢边界保留原参考线；压力参考与结构支撑共用价格标签。
  for (const level of model.keyLevels || []) {
    if (level.tone === "resistance") continue;
    if (model.layers?.supportResistance === false && level.tone === "support") continue;
    const color = level.tone === "support" ? colors.up : colors.blue;
    line(level.x1, level.y, level.x2, level.y, color, [3, 4], 1);
  }
  // 支撑、跌破待确认、失守与压力标签统一避让；价位和状态仍使用模型计算结果。
  const labeledLevels = [...(model.structureSupports || []), ...(model.keyLevels || []).filter((level) => level.tone === "resistance")];
  if (model.layers?.supportResistance !== false && labeledLevels.length) {
    // 只缩放价位坐标，线宽、圆点和字体使用 CSS 像素，避免宽屏显示粗重。
    context.save();
    context.setTransform(pixelScale, 0, 0, pixelHeight / height, pixelScale * leftBleed, 0);
    const scaleX = width / model.width, scaleY = height / model.height;
    const labelHeight = 20, labelGap = 20;
    const labelTop = model.plotTop * scaleY + labelHeight / 2;
    // 右下角保留情景按钮的空间，低价位标签不会被按钮遮挡。
    const labelBottom = (model.priceBottom ?? model.plotBottom) * scaleY - labelHeight / 2 - 28;
    const priceLevels = labeledLevels.map((level) => ({ level, y: level.y * scaleY, labelY: level.y * scaleY }))
      .sort((a, b) => a.y - b.y);
    let previousY = labelTop - labelGap;
    for (const row of priceLevels) {
      row.labelY = Math.max(row.labelY, previousY + labelGap);
      previousY = row.labelY;
    }
    if (priceLevels[priceLevels.length - 1].labelY > labelBottom) {
      priceLevels[priceLevels.length - 1].labelY = labelBottom;
      for (let index = priceLevels.length - 2; index >= 0; index -= 1) {
        priceLevels[index].labelY = Math.min(priceLevels[index].labelY, priceLevels[index + 1].labelY - labelGap);
      }
    }
    for (const row of priceLevels) {
      const { level, y, labelY } = row;
      const broken = level.status === "broken";
      const pending = level.status === "pending-break";
      const resistance = level.tone === "resistance";
      const color = broken ? colors.text : resistance ? colors.down : pending ? colors.supportPending : colors.blue;
      const statusText = broken ? "失守" : resistance ? "压力" : pending ? "跌破待确认" : "支撑";
      const priceText = level.priceLabel || level.label.replace(/^(跌破待确认|支撑|失守)\s*/, "");
      const labelX = (level.labelX ?? level.x2 - 5) * scaleX;
      context.font = "500 12px Inter, ui-sans-serif, system-ui, sans-serif";
      const priceWidth = context.measureText(priceText).width;
      context.font = "400 12px Inter, ui-sans-serif, system-ui, sans-serif";
      const statusWidth = context.measureText(statusText).width;
      const labelWidth = statusWidth + 6 + priceWidth;
      const labelLeft = labelX - labelWidth;
      const startX = level.x1 * scaleX;
      const endX = Math.max(startX, Math.min(level.x2 * scaleX, labelLeft - 8));
      // 记录实际绘制范围，悬浮提示仍按模型坐标命中。
      level.labelWidth = (labelWidth + 4) / scaleX;
      level.labelX = labelX / scaleX;
      level.labelHeight = labelHeight / scaleY;
      level.labelY = labelY / scaleY;
      level.lineEndX = endX / scaleX;
      level.lineHitPadding = 5 / scaleY;
      context.globalAlpha = broken ? 0.58 : pending ? 0.85 : resistance ? 0.82 : (level.lineAlpha ?? 0.68);
      if (endX > startX) line(startX, y, endX, y, color, broken ? [4, 4] : pending ? [6, 3] : resistance ? [3, 4] : [], broken || resistance ? 1 : 1.25);
      if (Math.abs(labelY - y) > 2) line(endX, y, labelLeft - 4, labelY, color, [], 1);
      if (level.pointX != null) {
        const pointX = level.pointX * scaleX;
        if (pointX < startX) line(pointX, y, startX, y, color, [2, 3], 1);
        context.beginPath(); context.arc(pointX, level.pointY * scaleY, 2.5, 0, Math.PI * 2);
        context.fillStyle = colors.panel; context.globalAlpha = 1; context.fill();
        context.strokeStyle = color; context.lineWidth = 1.25; context.globalAlpha = 0.8; context.stroke();
      }
      context.globalAlpha = 1;
      context.fillStyle = colors.panel;
      context.fillRect(labelLeft - 4, labelY - labelHeight / 2, labelWidth + 4, labelHeight);
      context.textAlign = "left"; context.textBaseline = "middle";
      context.fillStyle = color;
      context.globalAlpha = broken ? 0.85 : pending ? 1 : 0.9;
      context.fillText(statusText, labelLeft, labelY);
      context.font = "500 12px Inter, ui-sans-serif, system-ui, sans-serif";
      context.fillStyle = colors.textPrimary; context.globalAlpha = broken ? 0.7 : 0.85;
      context.fillText(priceText, labelLeft + statusWidth + 6, labelY);
    }
    context.restore();
  }
  if (model.layers?.signals === true) {
    // 只缩放信号坐标；标记、边线和字号固定为CSS像素，不改变信号数据与原布局。
    context.save();
    context.setTransform(pixelScale, 0, 0, pixelHeight / height, pixelScale * leftBleed, 0);
    const scaleX = width / model.width, scaleY = height / model.height;
    const occupiedLabels = [];
    for (const signal of model.signals || []) {
      signal.renderedMarker = null;
      signal.renderedLabel = null;
      const invalidated = signal.invalidated, confirmed = signal.confirmed === true || signal.status === "confirmed";
      const color = invalidated ? colors.text : signal.side === "buy" ? colors.up : colors.down;
      const markerX = signal.x * scaleX;
      const markerY = (Number.isFinite(signal.markerY) ? signal.markerY : signal.y) * scaleY;
      context.setLineDash([]); context.lineWidth = 1.15; context.strokeStyle = color;
      context.globalAlpha = invalidated ? 0.65 : 0.95;
      context.beginPath();
      if (invalidated) {
        context.arc(markerX, markerY, 3.5, 0, Math.PI * 2);
        context.stroke();
        context.beginPath(); context.moveTo(markerX - 2.5, markerY + 2.5); context.lineTo(markerX + 2.5, markerY - 2.5); context.stroke();
      } else {
        const direction = signal.side === "buy" ? 1 : -1;
        context.moveTo(markerX, markerY + direction * 4.5);
        context.lineTo(markerX - 4, markerY - direction * 2.5);
        context.lineTo(markerX + 4, markerY - direction * 2.5); context.closePath();
        context.fillStyle = confirmed ? color : colors.panel; context.fill();
        context.stroke();
      }
      signal.renderedMarker = { x: signal.x, y: markerY / scaleY, hitX: 9 / scaleX, hitY: 9 / scaleY };
      signal.renderedLabel = null;
      if (signal.labelVisible === false) continue;
      const title = signal.type || signal.label.replace(/\s*·\s*已失效$/, "");
      const status = invalidated ? "失效" : signal.status === "risk_triggered" ? "失守" : confirmed ? "确认" : "待确认";
      context.font = "500 12px Inter, ui-sans-serif, system-ui, sans-serif";
      const titleWidth = context.measureText(title).width;
      context.font = "400 10px Inter, ui-sans-serif, system-ui, sans-serif";
      const statusWidth = context.measureText(status).width;
      const labelWidth = titleWidth + statusWidth + 20, labelHeight = 20;
      const minimumX = model.plotLeft * scaleX + labelWidth / 2 + 2;
      const maximumX = model.historyRight * scaleX - labelWidth / 2 - 2;
      const preferredX = (Number.isFinite(signal.labelX) ? signal.labelX : signal.x) * scaleX;
      const labelX = minimumX <= maximumX ? Math.max(minimumX, Math.min(maximumX, preferredX)) : (minimumX + maximumX) / 2;
      const preferredY = (Number.isFinite(signal.labelY) ? signal.labelY : signal.y + (signal.side === "buy" ? 16 : -10)) * scaleY - 4;
      const minimumY = model.plotTop * scaleY + 12, maximumY = (model.priceBottom ?? model.plotBottom) * scaleY - 12;
      let labelY = Math.max(minimumY, Math.min(maximumY, preferredY));
      const baseY = labelY, direction = labelY >= markerY ? 1 : -1;
      const overlapsLabel = (candidateY) => occupiedLabels.some((box) => Math.abs(labelX - box.x) < (labelWidth + box.width) / 2 + 3
        && Math.abs(candidateY - box.y) < (labelHeight + box.height) / 2 + 3);
      // 只调整绘制框：扩大字号后仍保留3px标签间距，不回写信号坐标。
      for (let step = 1; overlapsLabel(labelY) && step <= Math.ceil((maximumY - minimumY) / 24); step += 1) {
        const candidates = [baseY + direction * step * 24, baseY - direction * step * 24];
        const availableY = candidates.find((candidateY) => candidateY >= minimumY && candidateY <= maximumY && !overlapsLabel(candidateY));
        if (availableY != null) { labelY = availableY; break; }
      }
      occupiedLabels.push({ x: labelX, y: labelY, width: labelWidth, height: labelHeight });
      const left = labelX - labelWidth / 2, top = labelY - labelHeight / 2, right = left + labelWidth, bottom = top + labelHeight;
      // 避让后离端点较远的标签使用淡连接线，便于对应K线位置。
      if (Math.hypot(labelX - markerX, labelY - markerY) > 24) {
        const direction = labelY >= markerY ? 1 : -1;
        context.globalAlpha = invalidated ? 0.16 : 0.24;
        line(markerX, markerY + direction * 6, labelX, labelY - direction * 10, color, [], 0.75);
      }
      context.beginPath(); context.moveTo(left + 4, top);
      context.arcTo(right, top, right, bottom, 4); context.arcTo(right, bottom, left, bottom, 4);
      context.arcTo(left, bottom, left, top, 4); context.arcTo(left, top, right, top, 4); context.closePath();
      context.fillStyle = colors.panel; context.globalAlpha = 0.97; context.fill();
      context.fillStyle = color; context.globalAlpha = invalidated ? 0.03 : 0.06; context.fill();
      context.strokeStyle = color; context.lineWidth = 0.75; context.globalAlpha = invalidated ? 0.18 : 0.28; context.stroke();
      context.textAlign = "left"; context.textBaseline = "middle";
      context.font = "500 12px Inter, ui-sans-serif, system-ui, sans-serif";
      context.fillStyle = color; context.globalAlpha = 1; context.fillText(title, left + 7, labelY);
      context.font = "400 10px Inter, ui-sans-serif, system-ui, sans-serif";
      context.fillStyle = colors.textPrimary; context.globalAlpha = invalidated ? 0.62 : 0.72; context.fillText(status, left + 13 + titleWidth, labelY);
      // 保存真实绘制框供悬浮命中；原labelX/labelY、日期、价格和状态保持不变。
      signal.renderedLabel = { x: labelX / scaleX, y: labelY / scaleY, width: labelWidth / scaleX, height: labelHeight / scaleY };
    }
    context.restore();
  }
  // AF StochZ overlays confirmed price pivots after the oscillator condition.
  // The marker is painted separately from Chan signals so either layer can be
  // inspected without changing the other layer's visibility.
  if (model.layers?.afStochZ === true) for (const signal of model.afSignals || []) {
    const color = signal.divergence === "hidden"
      ? signal.side === "buy" ? colors.blue : colors.amber
      : signal.side === "buy" ? colors.up : colors.down;
    const markerY = Number.isFinite(signal.markerY) ? signal.markerY : signal.y;
    const buy = signal.side === "buy";
    context.beginPath();
    context.moveTo(signal.x, markerY + (buy ? 7 : -7));
    context.lineTo(signal.x - 6, markerY + (buy ? -3 : 3));
    context.lineTo(signal.x + 6, markerY + (buy ? -3 : 3));
    context.closePath();
    context.fillStyle = color; context.globalAlpha = 0.92; context.fill(); context.globalAlpha = 1;
    const labelLines = signal.labelLines || (signal.label || (buy ? "BUY" : "SELL")).split("\n");
    const fontSize = model.mobileChart ? 11 : 8;
    const lineHeight = fontSize + 2;
    context.font = `650 ${fontSize}px Inter, ui-sans-serif, system-ui, sans-serif`;
    context.textAlign = "center"; context.textBaseline = "middle";
    const textWidth = Math.max(...labelLines.map((labelLine) => typeof context.measureText === "function"
      ? context.measureText(labelLine).width : Array.from(labelLine).length * fontSize * 0.55));
    const labelWidth = Math.max(textWidth + 8, signal.hitLabelWidth || 0);
    const labelHeight = Math.max(signal.labelHeight || 0, labelLines.length * lineHeight + 4);
    const halfLabelWidth = labelWidth / 2;
    const labelMinX = model.plotLeft + halfLabelWidth + 2;
    const labelMaxX = model.historyRight - halfLabelWidth - 2;
    const labelX = labelMinX <= labelMaxX
      ? Math.max(labelMinX, Math.min(labelMaxX, signal.labelX ?? signal.x))
      : (model.plotLeft + model.historyRight) / 2;
    const plotTop = Number.isFinite(model.plotTop) ? model.plotTop : 0;
    const priceBottom = Number.isFinite(model.priceBottom) ? model.priceBottom : model.plotBottom;
    const labelMinY = plotTop + labelHeight / 2 + 2;
    const labelMaxY = priceBottom - labelHeight / 2 - 2;
    const preferredLabelY = Number.isFinite(signal.labelY) ? signal.labelY : markerY + (buy ? 22 : -18);
    const labelY = labelMinY <= labelMaxY
      ? Math.max(labelMinY, Math.min(labelMaxY, preferredLabelY))
      : (plotTop + priceBottom) / 2;
    const labelTop = labelY - labelHeight / 2;
    const firstLineY = labelTop + (labelHeight - labelLines.length * lineHeight) / 2 + lineHeight / 2;
    if (signal.labelConnector) {
      const labelEdgeY = labelY > markerY ? labelY - labelHeight / 2 : labelY + labelHeight / 2;
      context.beginPath();
      context.moveTo(signal.x, markerY + (labelEdgeY > markerY ? 8 : -8));
      context.lineTo(labelX, labelEdgeY);
      context.strokeStyle = color; context.lineWidth = 1; context.globalAlpha = 0.62; context.stroke(); context.globalAlpha = 1;
    }
    if (signal.labelBackground !== false) {
      context.globalAlpha = 0.96;
      context.fillStyle = color;
      context.fillRect(labelX - halfLabelWidth, labelTop, labelWidth, labelHeight);
      context.globalAlpha = 1;
      context.fillStyle = colors.panel;
      labelLines.forEach((labelLine, lineIndex) => context.fillText(labelLine, labelX, firstLineY + lineIndex * lineHeight));
    } else {
      context.strokeStyle = colors.panel; context.lineWidth = 3; context.lineJoin = "round";
      labelLines.forEach((labelLine, lineIndex) => context.strokeText(labelLine, labelX, firstLineY + lineIndex * lineHeight));
      context.fillStyle = color;
      labelLines.forEach((labelLine, lineIndex) => context.fillText(labelLine, labelX, firstLineY + lineIndex * lineHeight));
    }
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
  const dividerPositions = Array.isArray(model.indicatorDividers)
    ? model.indicatorDividers
    : [
      Number.isFinite(model.indicatorDividerY) ? model.indicatorDividerY
        : Number.isFinite(model.volumeBottom) && Number.isFinite(model.macdTop)
          ? model.volumeBottom + (model.macdTop - model.volumeBottom) / 3 : NaN,
      Number.isFinite(model.dmiDividerY) ? model.dmiDividerY
        : Number.isFinite(model.macdBottom) && Number.isFinite(model.dmiTop)
          ? model.macdBottom + (model.dmiTop - model.macdBottom) / 3 : NaN,
    ];
  for (const dividerY of dividerPositions) {
    if (Number.isFinite(dividerY)) line(model.plotLeft, dividerY, model.plotRight, dividerY, colors.fieldLine, [], 1.5);
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
  // MFI 固定使用 0–100 范围，白线为个股，淡粉线为标普，均线按方向变色。
  if (Number.isFinite(model.mfiTop) && Number.isFinite(model.mfiBottom)) {
    const mfiHeight = model.mfiBottom - model.mfiTop;
    const mfiY = (value) => model.mfiTop + (100 - value) / 100 * mfiHeight;
    line(model.plotLeft, model.mfiTop, model.plotRight, model.mfiTop, colors.grid, [2, 4], 1);
    for (const value of [20, 50, 80]) {
      const y = mfiY(value);
      line(model.plotLeft, y, model.plotRight, y, value === 50 ? colors.fieldLine : colors.amber, [4, 3], value === 50 ? 0.8 : 1.1);
      label(String(value), model.plotLeft - 8, y + 3, {
        align: "right", font: `${model.mobileChart ? 11 : 9}px Inter, ui-sans-serif, system-ui, sans-serif`,
        color: value === 50 ? colors.text : colors.amber,
      });
    }
    const drawMfiLine = (key, color, lineWidth = 1.5) => {
      context.beginPath();
      let started = false;
      for (const candle of model.candles || []) {
        const rawValue = candle.mfi?.[key];
        if (rawValue == null || rawValue === "") { started = false; continue; }
        const value = Number(rawValue);
        if (!Number.isFinite(value)) { started = false; continue; }
        const y = mfiY(value);
        if (started) context.lineTo(candle.x, y);
        else { context.moveTo(candle.x, y); started = true; }
      }
      if (!started) return;
      context.strokeStyle = color;
      context.lineWidth = lineWidth;
      context.setLineDash([]);
      context.stroke();
    };
    drawMfiLine("value", colors.textPrimary, model.mobileChart ? 2 : 1.5);
    drawMfiLine("benchmark", "#d58aa6", 1.1);
    let previousTrend = null;
    for (const candle of model.candles || []) {
      const rawValue = candle.mfi?.average;
      if (rawValue == null || rawValue === "") { previousTrend = null; continue; }
      const value = Number(rawValue);
      if (!Number.isFinite(value)) { previousTrend = null; continue; }
      const current = { x: candle.x, y: mfiY(value), trend: candle.mfi?.trend };
      if (previousTrend && previousTrend.trend === current.trend) {
        context.beginPath();
        context.moveTo(previousTrend.x, previousTrend.y);
        context.lineTo(current.x, current.y);
        context.strokeStyle = current.trend === "up" ? colors.down : current.trend === "down" ? colors.up : colors.axis;
        context.lineWidth = model.mobileChart ? 2 : 1.4;
        context.stroke();
      }
      previousTrend = current;
    }
    for (const event of model.mfiEvents || []) {
      const color = event.side === "buy" ? colors.down : colors.up;
      const radius = model.mobileChart ? 4 : 3;
      context.beginPath();
      if (event.side === "buy") {
        context.moveTo(event.x, event.y - radius);
        context.lineTo(event.x - radius, event.y + radius);
        context.lineTo(event.x + radius, event.y + radius);
      } else {
        context.moveTo(event.x, event.y + radius);
        context.lineTo(event.x - radius, event.y - radius);
        context.lineTo(event.x + radius, event.y - radius);
      }
      context.closePath();
      context.fillStyle = color;
      context.fill();
    }
    const latestCandle = latestIndicatorCandle(model);
    const header = indicatorHeaderText(latestCandle);
    const value = Number(latestCandle?.mfi?.value);
    const mfiColor = Number.isFinite(value) ? (value >= 80 ? colors.down : value <= 20 ? colors.up : colors.text) : colors.text;
    drawInlineIndicatorHeader(context, model, model.plotLeft, model.mfiTop - 6,
      Math.max(1, model.plotRight - model.plotLeft), header.mfi, header.mfiInterpretation,
      header.mfiCompact, header.mfiStatus, colors.text, mfiColor);
    line(model.plotLeft, model.mfiBottom, model.plotRight, model.mfiBottom, colors.axis, [], 1);
  }
  if (model.rsi) {
    const rsiY = (value) => model.rsiTop + (100 - value) / 100 * (model.rsiBottom - model.rsiTop);
    for (const value of [70, 50, 30]) {
      const y = rsiY(value);
      line(model.plotLeft, y, model.plotRight, y, colors.grid, [3, 4]);
      label(String(value), model.plotLeft - 8, y + 4, { align: "right" });
    }
    context.save();
    context.beginPath();
    context.rect(model.plotLeft, model.rsiTop, model.historyRight - model.plotLeft, model.rsiBottom - model.rsiTop);
    context.clip();
    let previous = null;
    for (const point of model.rsi.series) {
      if (previous && Number.isFinite(point.value) && Number.isFinite(previous.value)) {
        line(previous.x, rsiY(previous.value), point.x, rsiY(point.value), colors.blue, point.live ? [3, 3] : [], 1.4);
      }
      previous = point;
    }
    let lastLabelX = -Infinity;
    for (const event of model.rsi.events) {
      const color = event.invalidated ? colors.text : event.side === "top" ? colors.down : colors.up;
      const y1 = rsiY(event.from.rsi), y2 = rsiY(event.to.rsi);
      line(event.x1, y1, event.x2, y2, color, event.status === "watch" || event.invalidated ? [3, 3] : [], 1.6);
      if (event.x - lastLabelX < (model.mobileChart ? 72 : 94) && event !== model.rsi.events.at(-1)) continue;
      const caption = `${event.side === "top" ? "顶背离" : "底背离"}${event.status === "watch" ? " · 观察" : event.invalidated ? " · 失效" : ""}`;
      const font = `${model.mobileChart ? 10 : 11}px Inter, ui-sans-serif, system-ui, sans-serif`;
      const rightAligned = event.x > model.historyRight - 92;
      const textX = rightAligned ? event.x - 2 : event.x + 2;
      label(caption, textX, Math.max(model.rsiTop + 11, Math.min(model.rsiBottom - 3, y2 - 4)),
        { color, align: rightAligned ? "right" : "left", font });
      lastLabelX = event.x;
    }
    context.restore();
    label("RSI (14)", model.plotLeft, model.rsiTop - 6);
    line(model.plotLeft, model.rsiBottom, model.plotRight, model.rsiBottom, colors.axis);
  }
  if (model.rs) {
    const values = model.rs.series.filter(Boolean).map((point) => point.value);
    const range = Math.max(0.5, ...values.map((value) => Math.abs(value - 100))) * 1.08;
    const rsY = (value) => model.rsTop + (1 - (value - 100) / range) * (model.rsBottom - model.rsTop) / 2;
    const baseline = rsY(100);
    line(model.plotLeft, baseline, model.plotRight, baseline, colors.axis, [4, 4]);
    label("100", model.plotLeft - 8, baseline + 4, { align: "right" });
    let previous = null;
    for (const point of model.rs.series) {
      if (point && previous) line(previous.x, rsY(previous.value), point.x, rsY(point.value), colors.blue, point.live ? [3, 3] : [], 1.5);
      if (point && (!previous || point.changed)) {
        context.fillStyle = point.changed ? point.state === "outperform" ? colors.up : colors.down : colors.blue;
        const size = point.changed ? 4 : 3;
        context.fillRect(point.x - size / 2, rsY(point.value) - size / 2, size, size);
      }
      previous = point;
    }
    label(model.rs.symbol ? `RS / ${model.rs.symbol}` : "RS", model.plotLeft, model.rsTop - 6);
    line(model.plotLeft, model.rsBottom, model.plotRight, model.rsBottom, colors.axis);
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
  // 按钮保持透明覆盖层，以保留键盘聚焦与原生点击行为。
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
    context.fillText("\ue73d", toggleX + toggleSize / 2, toggleY + toggleSize / 2);
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

// 鼠标拖动平移可见K线范围；绘图区滚轮和双指捏合围绕指针位置缩放。
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
    const visibleBars = Math.max(1, Number(model.visibleBars) || model.candles.length);
    const slot = (model.historyRight - model.plotLeft) / visibleBars;
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
    const point = localPoint(event);
    const model = state.model;
    if (!point || !model?.candles?.length
      || point.x < model.plotLeft || point.x > model.historyRight
      || point.y < model.plotTop || point.y > model.plotBottom) return;
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
  // 在 SVG 图层前捕获滚轮，避免指标层截获缩放事件。
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
  // 异步行情绘制时，容器可能尚未生成或已经移除；跳过本次绘制，等待下一次有效更新。
  if (!target) return;
  const positiveBoth = options.positiveBoth === true;
  if (!points.length || (!positiveBoth && points.every((point) => !(Math.abs(point[positiveKey]) + Math.abs(point[negativeKey]))))) { target.innerHTML = `<div class="chart-empty">${emptyMessage}</div>`; return; }
  // 按容器实际像素绘制：viewBox 与元素 1:1，手机窄屏不会再把柱子和坐标文字整体等比缩小。
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width); target.dataset.chartHeight = String(height);
  // 成交量/持仓量图按行情软件样式把数值列放在右侧，Gamma 图保留左侧数值列。
  const axisOnRight = options.axis === "right";
  const pad = positiveBoth ? { left: 62, right: 18, top: 14, bottom: 48 } : axisOnRight ? { left: 16, right: 56, top: 14, bottom: 26 } : { left: 48, right: 12, top: 14, bottom: 26 }; const innerWidth = width - pad.left - pad.right; const innerHeight = height - pad.top - pad.bottom;
  // 上下各留一条固定高度的「标签通道」：柱子最高只顶到通道内侧的标高线，柱端文字与方向角标住在通道里，不会再压到坐标刻度。
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16)); const scaleTop = pad.top + gutter; const scaleBottom = height - pad.bottom - gutter;
  const baseline = positiveBoth ? height - pad.bottom : pad.top + innerHeight / 2;
  const maxBarHeight = positiveBoth ? baseline - scaleTop : innerHeight / 2 - gutter;
  const rawMaxValue = Math.max(0, ...points.map((point) => positiveBoth
    ? Math.abs(Number(point[positiveKey]) || 0) + Math.abs(Number(point[negativeKey]) || 0)
    : Math.max(Math.abs(point[positiveKey]), Math.abs(point[negativeKey]))));
  // 正赔付图用总柱高定标，四个等距区间并给最高柱留余量。
  const tickSize = Math.max(rawMaxValue * 1.05 / 4, 0.000001);
  const tickMagnitude = 10 ** Math.floor(Math.log10(tickSize));
  const tickStep = [1, 2, 2.5, 5, 10].find((step) => step * tickMagnitude >= tickSize) * tickMagnitude;
  const maxValue = positiveBoth ? tickStep * 4 : Math.max(rawMaxValue, 1);
  const slot = innerWidth / points.length; const barWidth = Math.max(3, Math.min(26, slot * 0.68));
  const yLabel = formatChartValue(maxValue, 2, unit);
  // 左侧数值列右边界：最左行权价的柱端文字压到数值时向右避让。
  const axisColumnWidth = 6 + Math.max(markerLabelWidth(yLabel, 400), markerLabelWidth(`-${yLabel}`, 400));
  const labelColumnRight = axisOnRight ? 0 : axisColumnWidth + 4;
  // 刻度文字按可用宽度决定数量：桌面宽屏约每 84px 一个，手机上自动减少避免重叠。
  const labelEvery = Math.max(1, Math.ceil(points.length / Math.max(2, Math.floor(innerWidth / 84))));
  const painLabelCount = Math.min(points.length, Math.max(2, Math.floor(innerWidth / 130) + 1));
  const painLabelIndices = new Set(Array.from({ length: painLabelCount }, (_, index) => Math.round(index * (points.length - 1) / Math.max(1, painLabelCount - 1))));
  const bars = points.map((point, index) => {
    const centerX = pad.left + index * slot + slot / 2;
    const positive = Number(point[positiveKey]) || 0;
    const negative = Number(point[negativeKey]) || 0;
    const positiveHeight = Math.abs(positive) / maxValue * maxBarHeight;
    const negativeHeight = Math.abs(negative) / maxValue * maxBarHeight;
    const showLabel = positiveBoth ? painLabelIndices.has(index) : index % labelEvery === 0;
    const label = showLabel ? `<text class="chart-label" x="${centerX}" y="${positiveBoth ? baseline + 22 : height - 8}" text-anchor="${positiveBoth && index === 0 ? "start" : positiveBoth && index === points.length - 1 ? "end" : "middle"}">${positiveBoth ? formatMoney(point.strike).replace(/\.00$/, "") : formatMoney(point.strike)}</text>` : "";
    if (positiveBoth) {
      const stackWidth = Math.min(18, slot * 0.68);
      const stackX = centerX - stackWidth / 2;
      return `<rect class="chart-max-pain-put" x="${stackX}" y="${baseline - negativeHeight}" width="${stackWidth}" height="${negativeHeight}"></rect><rect class="chart-max-pain-call" x="${stackX}" y="${baseline - negativeHeight - positiveHeight}" width="${stackWidth}" height="${positiveHeight}"></rect>${label}`;
    }
    const x = centerX - barWidth / 2;
    return `<rect class="chart-call" x="${x}" y="${baseline - positiveHeight}" width="${barWidth}" height="${positiveHeight}" rx="1"></rect><rect class="chart-put" x="${x}" y="${baseline}" width="${barWidth}" height="${negativeHeight}" rx="1"></rect>${label}`;
  }).join("");
  const placedPriceLabels = [];
  const priceMarkers = positiveBoth ? (options.priceMarkers || []).filter((marker) => marker.value != null && Number.isFinite(Number(marker.value))).map((marker) => {
    const position = strikePosition(points, Number(marker.value));
    if (position == null) return "";
    const x = pad.left + position * slot + slot / 2;
    const label = formatMoney(Number(marker.value)).replace(/\.00$/, "");
    const halfLabel = markerLabelWidth(label) * 0.7 + 4;
    const labelX = Math.min(Math.max(x, pad.left + halfLabel), width - pad.right - halfLabel);
    // 价格靠近时错行显示，两条参考线仍准确落在原价位上。
    const overlaps = placedPriceLabels.some((previous) => Math.abs(previous.x - labelX) < previous.halfWidth + halfLabel + 8);
    const labelY = scaleTop - (overlaps ? 6 : 28);
    placedPriceLabels.push({ x: labelX, halfWidth: halfLabel });
    return `<line class="${marker.className}" x1="${x}" x2="${x}" y1="${scaleTop}" y2="${baseline}"/><text class="${marker.className}-label chart-price-marker-label" x="${labelX}" y="${labelY}" text-anchor="middle">${label}<title>${marker.label} ${label}</title></text>`;
  }).join("") : "";
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
  const axisLabels = positiveBoth
    ? Array.from({ length: 5 }, (_, index) => `<text class="chart-label" x="${pad.left - 14}" y="${baseline - maxBarHeight * index / 4 + 4}" text-anchor="end">${index ? formatChartValue(maxValue * index / 4, 2, unit) : "0"}</text>`).join("")
    : axisOnRight
    ? `<text class="chart-label" x="${width - 6}" y="${scaleTop + 4}" text-anchor="end">${yLabel}</text><text class="chart-label" x="${width - 6}" y="${halfTop + 4}" text-anchor="end">${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${baseline + 4}" text-anchor="end">0</text><text class="chart-label" x="${width - 6}" y="${halfBottom + 4}" text-anchor="end">-${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${scaleBottom + 4}" text-anchor="end">-${yLabel}</text>`
    : `<text class="chart-label" x="4" y="${scaleTop + 4}">${yLabel}</text><text class="chart-label" x="4" y="${baseline + 4}">0</text><text class="chart-label" x="4" y="${scaleBottom + 4}">-${yLabel}</text>`;
  const gridLines = positiveBoth
    ? [1, 2, 3].map((index) => `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline - maxBarHeight * index / 4}" y2="${baseline - maxBarHeight * index / 4}"/>`).join("")
    : axisOnRight ? `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfTop}" y2="${halfTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfBottom}" y2="${halfBottom}"/>` : "";
  // 右轴图表不再画角落方向文字，方向改由图例说明，避免与右侧数值列抢位。
  const cornerLabels = axisOnRight || positiveBoth ? "" : `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">看涨 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">看跌 ↓</text>`;
  const axisTitle = positiveBoth ? `<text class="chart-label chart-axis-title" x="${width - pad.right}" y="${height - 4}" text-anchor="end">（行权价）</text>` : "";
  const tagElements = options.crosshairTags ? `<line class="chart-crosshair-h" x1="${pad.left}" x2="${width - pad.right}" y1="0" y2="0" visibility="hidden"/><g class="chart-tag chart-tag-x" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g><g class="chart-tag chart-tag-y" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g>` : "";
  const svg = `<svg viewBox="0 0 ${width} ${height}" role="img" tabindex="0" aria-label="${options.ariaLabel || target.getAttribute("aria-label") || "期权分布图"}">${gridLines}<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleTop}" y2="${scaleTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/>${positiveBoth ? "" : `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleBottom}" y2="${scaleBottom}"/>`}<line class="chart-zero" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/>${axisLabels}${gammaFlipMarker}${bars}${priceMarkers}${markers}<line class="chart-crosshair" x1="0" x2="0" y1="${pad.top}" y2="${height - pad.bottom}" visibility="hidden"/>${tagElements}${cornerLabels}${axisTitle}</svg><div class="chart-tooltip" hidden></div>`; target.innerHTML = svg;

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
    const isGamma = targetId === "gex-chart" && !positiveBoth;
    const net = isGamma ? positive + negative : positive - negative;
    const valueLabel = isGamma ? " GEX" : (options.valueLabel || "");
    const rows = [
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot call"></i>看涨${valueLabel}</span><strong>${formatChartValue(Math.abs(positive), 2, unit)}</strong></div>`,
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot put"></i>看跌${valueLabel}</span><strong>${formatChartValue(Math.abs(negative), 2, unit)}</strong></div>`,
    ];
    if (isGamma) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>净 GEX</span><strong>${formatChartValue(net, 2, unit)}</strong></div>`);
      if (options.spot != null) rows.unshift(`<div class="chart-tooltip-sub">行情价 ${formatMoney(options.spot)}</div>`);
    } else if (positiveBoth) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>总${options.valueLabel || "数值"}</span><strong>${formatChartValue(Math.abs(positive) + Math.abs(negative), 2, unit)}</strong></div>`);
      if (options.spot != null) rows.unshift(`<div class="chart-tooltip-sub">当前价格 ${formatMoney(options.spot)}</div>`);
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
function renderStrikeGexProfile(targetId, points, options = {}) {
  const target = document.getElementById(targetId);
  if (!target) return;
  const currentPoints = points || [];
  const hasGex = currentPoints.some((point) => point.callGex || point.putGex || point.callVolumeGex || point.putVolumeGex);
  if (!currentPoints.length || !hasGex) {
    target.innerHTML = "";
    target.textContent = options.emptyMessage || "当前期权链缺少可用 Gamma";
    target.dataset.chartWidth = "";
    target.dataset.chartHeight = "";
    target.dataset.profileScrollKey = "";
    target.scrollTop = 0;
    return;
  }

  const { width } = chartContentBox(target);
  const rowHeight = 26;
  const footerHeight = 42;
  const currentByStrike = new Map(currentPoints.map((point) => [point.strike, point]));
  const allStrikes = [...currentByStrike.keys()].sort((left, right) => right - left);
  const nearestStrikeIndex = (value, strikeList = allStrikes) => {
    const targetStrike = Number(value);
    if (!Number.isFinite(targetStrike) || !strikeList.length) return -1;
    let nearest = 0;
    for (let index = 1; index < strikeList.length; index += 1) {
      if (Math.abs(strikeList[index] - targetStrike) < Math.abs(strikeList[nearest] - targetStrike)) nearest = index;
    }
    return nearest;
  };
  const anchorIndexes = [options.spot, options.zeroGamma, options.positiveStrike, options.negativeStrike]
    .filter((value) => value != null && Number.isFinite(Number(value)))
    .map((value) => nearestStrikeIndex(value))
    .filter((index) => index >= 0);
  const firstAnchorIndex = anchorIndexes.length ? Math.min(...anchorIndexes) : 0;
  const lastAnchorIndex = anchorIndexes.length ? Math.max(...anchorIndexes) : allStrikes.length - 1;
  const firstVisibleIndex = Math.max(0, firstAnchorIndex - 10);
  const lastVisibleIndex = Math.min(allStrikes.length - 1, lastAnchorIndex + 10);
  const strikes = allStrikes.slice(firstVisibleIndex, lastVisibleIndex + 1);
  const visibleStrikes = new Set(strikes);
  const maxDataValue = Math.max(
    0.000001,
    ...currentPoints.filter((point) => visibleStrikes.has(point.strike)).flatMap((point) => [point.callGex, point.putGex, point.callVolumeGex, point.putVolumeGex].map((value) => Math.abs(Number(value) || 0))),
  );
  const rawMaximumBillions = maxDataValue / 1000;
  const magnitude = 10 ** Math.floor(Math.log10(rawMaximumBillions));
  const axisMaximumBillions = Math.max(magnitude * 3, Math.ceil(rawMaximumBillions / (magnitude * 3)) * magnitude * 3);
  const axisMaximum = axisMaximumBillions * 1000;
  const strikeLabelGutter = Math.min(80, Math.max(52, width * 0.075));
  const plotLeft = strikeLabelGutter + 4;
  const plotRight = width - 24;
  const plotWidth = plotRight - plotLeft;
  const halfBarWidth = Math.max(24, plotWidth / 2 - 4);
  const zeroX = plotLeft + plotWidth / 2;
  const strikeLabelX = plotLeft - 8;
  const xOf = (value) => zeroX + (Number(value) || 0) / axisMaximum * halfBarWidth;
  const barLength = (value) => Math.abs(xOf(value) - zeroX);
  const referenceLines = [
    { value: options.spot, className: "chain-profile-level-spot" },
    { value: options.zeroGamma, className: "chain-profile-level-flip" },
    { value: options.positiveStrike, className: "chain-profile-level-positive" },
    { value: options.negativeStrike, className: "chain-profile-level-negative" },
  ].map((line) => {
    const value = Number(line.value);
    if (line.value == null || !Number.isFinite(value) || value > strikes[0] || value < strikes[strikes.length - 1]) return "";
    const index = nearestStrikeIndex(value, strikes);
    if (index < 0) return "";
    const y = index * rowHeight + rowHeight / 2;
    return `<line class="${line.className}" x1="${plotLeft}" x2="${plotRight}" y1="${y}" y2="${y}"/>`;
  }).join("");
  const bars = strikes.map((strike, index) => {
    const point = currentByStrike.get(strike);
    if (!point) return "";
    const centerY = index * rowHeight + rowHeight / 2;
    const callOpenInterest = Number(point.callGex) || 0;
    const putOpenInterest = Number(point.putGex) || 0;
    const callVolume = Number(point.callVolumeGex) || 0;
    const putVolume = Number(point.putVolumeGex) || 0;
    const title = `行权价 ${formatMoney(strike)} · 看涨持仓量 GEX ${formatChartValue(callOpenInterest, 2, "M")} · 看跌持仓量 GEX ${formatChartValue(putOpenInterest, 2, "M")} · 看涨成交量 GEX ${formatChartValue(callVolume, 2, "M")} · 看跌成交量 GEX ${formatChartValue(putVolume, 2, "M")}`;
    const rect = (value, className, label) => {
      const x = value < 0 ? xOf(value) : zeroX;
      return `<rect class="${className}" x="${x}" y="${centerY - 3}" width="${barLength(value)}" height="6" rx="1"><title>${label}</title></rect>`;
    };
    // 同侧指标共用行中心和6px厚度，深色持仓量覆盖亮色成交量。
    return `${rect(callVolume, "chain-profile-volume-positive", title)}${rect(putVolume, "chain-profile-volume-negative", title)}${rect(callOpenInterest, "chain-profile-oi-positive", title)}${rect(putOpenInterest, "chain-profile-oi-negative", title)}`;
  }).join("");
  const gridLines = Array.from({ length: 7 }, (_, index) => {
    const tick = (index - 3) / 3 * axisMaximum;
    const x = xOf(tick);
    const className = index === 3 ? "chain-profile-zero" : "chain-profile-grid";
    return `<line class="${className}" x1="${x}" x2="${x}" y1="0" y2="${strikes.length * rowHeight}"/>`;
  }).join("");
  const rowLines = strikes.map((strike, index) => {
    const y = index * rowHeight;
    return `<line class="chain-profile-row" x1="0" x2="${width}" y1="${y + rowHeight - 1}" y2="${y + rowHeight - 1}"/><text class="chain-profile-strike" x="${strikeLabelX}" y="${y + rowHeight / 2 + 4}" text-anchor="end">${formatMoney(strike)}</text>`;
  }).join("");
  const plotDivider = `<line class="chain-profile-label-divider" x1="${plotLeft}" x2="${plotLeft}" y1="0" y2="${strikes.length * rowHeight}"/>`;
  const currentLegend = [
    { className: "chain-profile-oi-positive", label: "看涨持仓量 GEX" },
    { className: "chain-profile-oi-negative", label: "看跌持仓量 GEX" },
    { className: "chain-profile-volume-positive", label: "看涨成交量 GEX" },
    { className: "chain-profile-volume-negative", label: "看跌成交量 GEX" },
  ];
  const levelLegend = [
    { className: "chain-profile-spot-key", label: "现价" },
    { className: "chain-profile-flip-key", label: "零伽马" },
    { className: "chain-profile-positive-key", label: "主要正伽马" },
    { className: "chain-profile-negative-key", label: "主要负伽马" },
  ];
  const legendGroup = (label, items) => `<section class="chain-profile-legend-group"><h3>${label}</h3><div class="chain-profile-legend-items">${items.map((item) => `<span class="chain-profile-legend-item"><i class="${item.className}"></i>${item.label}</span>`).join("")}</div></section>`;
  const legendItems = [
    legendGroup("关键价位", levelLegend),
    legendGroup("当前伽马敞口", currentLegend),
  ].join("");
  const axisPrecision = Math.min(6, Math.max(1, Math.ceil(-Math.log10(magnitude))));
  const axisLabels = Array.from({ length: 7 }, (_, index) => {
    const tick = (index - 3) / 3 * axisMaximumBillions;
    const label = tick === 0 ? "0" : `${tick < 0 ? "-" : ""}${formatNumber(Math.abs(tick), axisPrecision)}`;
    const x = xOf((index - 3) / 3 * axisMaximum);
    const anchor = index === 0 ? "start" : index === 6 ? "end" : "middle";
    return `<line class="chain-profile-axis-tick" x1="${x}" x2="${x}" y1="0" y2="5"/><text class="chain-profile-axis-label" x="${x}" y="17" text-anchor="${anchor}">${label}</text>`;
  }).join("");
  const axisFooter = `<svg class="chain-profile-axis-footer" viewBox="0 0 ${width} ${footerHeight}" width="100%" height="${footerHeight}" aria-hidden="true"><line class="chain-profile-axis-baseline" x1="${plotLeft}" x2="${plotRight}" y1="1" y2="1"/>${axisLabels}<text class="chain-profile-axis-title" x="${zeroX}" y="36" text-anchor="middle">伽马敞口（十亿美元）</text></svg>`;
  const scrollKey = String(options.scrollKey || "");
  target.dataset.chartWidth = String(width);
  target.innerHTML = `<div class="chain-profile-axis-header"><h2 class="chain-profile-chart-title"></h2><div class="chain-profile-legend">${legendItems}</div></div><svg class="chain-profile-svg" viewBox="0 0 ${width} ${strikes.length * rowHeight}" width="100%" height="${strikes.length * rowHeight}" role="img" aria-label="按行权价显示的 GEX 分布图" tabindex="0">${gridLines}${referenceLines}${bars}${rowLines}${plotDivider}</svg>${axisFooter}`;
  target.querySelector(".chain-profile-chart-title").textContent = options.title || "按行权价伽马敞口";
  const headerHeight = target.querySelector(".chain-profile-axis-header")?.offsetHeight || 42;
  target.dataset.chartHeight = String(headerHeight + strikes.length * rowHeight + footerHeight);
  if (target.clientHeight > 0 && target.dataset.profileScrollKey !== scrollKey) {
    const spotIndex = nearestStrikeIndex(options.spot);
    const spotPosition = spotIndex < 0 ? 0 : headerHeight + spotIndex * rowHeight + rowHeight / 2;
    target.scrollTop = Math.max(0, Math.round((spotPosition - target.clientHeight / 2) / rowHeight) * rowHeight);
    target.dataset.profileScrollKey = scrollKey;
  }
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
  const label = options.ariaLabel || target.getAttribute("aria-label") || "图表";
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
  renderStrikeGexProfile: renderStrikeGexProfile,
  renderLevelsChart: renderLevelsChart,
  renderDistributionSummary: renderDistributionSummary,
  renderChartSkeleton: renderChartSkeleton,
  chartContentBox: chartContentBox,
  showEmpty: showEmpty,
  clearSummary: clearSummary,
};
}(window));
