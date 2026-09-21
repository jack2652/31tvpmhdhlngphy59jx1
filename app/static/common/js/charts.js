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

// 鼠标移动事件可能远高于屏幕刷新率；每帧只处理最后一次坐标，避免连续重排 SVG 和提示框。
function scheduleChartPointerMove(chartSvg, onMove, hideTooltip) {
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
    pending = { clientX: event.clientX, clientY: event.clientY, buttons: event.buttons || 0 };
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
  chartSvg.addEventListener("pointermove", schedule);
  const stop = () => { cancel(); hideTooltip(); };
  chartSvg.addEventListener("pointerleave", stop);
  chartSvg.addEventListener("pointercancel", stop);
  return cancel;
}

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
  const bars = points.map((point, index) => { const x = pad.left + index * slot + (slot - barWidth) / 2; const positive = Number(point[positiveKey]) || 0; const negative = Number(point[negativeKey]) || 0; const positiveHeight = Math.abs(positive) / maxValue * maxBarHeight; const negativeHeight = Math.abs(negative) / maxValue * maxBarHeight; const label = index % labelEvery === 0 ? `<text class="chart-label" x="${x + barWidth / 2}" y="${height - 8}" text-anchor="middle">${formatMoney(point.strike)}</text>` : ""; return `<rect class="chart-call" x="${x}" y="${baseline - positiveHeight}" width="${barWidth}" height="${positiveHeight}" rx="1"><title>${formatMoney(point.strike)} 看涨 ${formatChartValue(positive, 2, unit)}</title></rect><rect class="chart-put" x="${x}" y="${baseline}" width="${barWidth}" height="${negativeHeight}" rx="1"><title>${formatMoney(point.strike)} 看跌 ${formatChartValue(Math.abs(negative), 2, unit)}</title></rect>${label}`; }).join("");
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
    const title = `${item.side === "up" ? "压力位" : "支撑位"} ${formatMoney(item.price)} · 强度 ${item.score.toFixed(2)}`;
    const showLabel = centerX - lastLabelX >= minLabelGap;
    if (showLabel) lastLabelX = centerX;
    const label = showLabel ? `<text class="chart-label" x="${centerX}" y="${height - 8}" text-anchor="middle">${formatMoney(item.price)}</text>` : "";
    return `<rect class="${className}" x="${centerX - barWidth / 2}" y="${y}" width="${barWidth}" height="${barHeight}" rx="1"><title>${title}</title></rect>${label}`;
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
  renderSignedChart: renderSignedChart,
  renderLevelsChart: renderLevelsChart,
  renderDistributionSummary: renderDistributionSummary,
  chartContentBox: chartContentBox,
  showEmpty: showEmpty,
  clearSummary: clearSummary,
};
}(window));
