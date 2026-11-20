package store

import (
	"database/sql"
	"errors"
	"time"

	"option-scope-go/internal/model"
)

const chainHistorySnapshotTolerance = 2 * time.Minute

var chainHistoryOffsets = [...]int{1, 5, 10, 15, 30}

func (s *SQLite) ChainHistory(symbol, expiration string, asOf time.Time) ([]model.ChainHistoryPoint, error) {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	points := make([]model.ChainHistoryPoint, 0, len(chainHistoryOffsets))
	for _, minutesAgo := range chainHistoryOffsets {
		targetAt := asOf.Add(-time.Duration(minutesAgo) * time.Minute)
		point := model.ChainHistoryPoint{
			MinutesAgo: minutesAgo,
			TargetAt:   targetAt,
		}
		fetchedTimestamp, fetchedAt, found, err := s.chainSnapshotAtOrBefore(symbol, expiration, targetAt)
		if err != nil {
			return nil, err
		}
		if !found {
			point.UnavailableReason = "目标时间附近没有可用的期权链快照"
			points = append(points, point)
			continue
		}
		point.FetchedAt = &fetchedAt
		spot, quoteFetchedAt, found, err := s.quoteAtOrBefore(symbol, fetchedTimestamp, fetchedAt)
		if err != nil {
			return nil, err
		}
		if !found {
			point.UnavailableReason = "期权链快照附近没有可用的标的价格"
			points = append(points, point)
			continue
		}
		rows, err := s.chainRowsAt(symbol, expiration, fetchedTimestamp)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			point.UnavailableReason = "该期权链快照没有可用合约"
			points = append(points, point)
			continue
		}
		point.QuoteFetchedAt = &quoteFetchedAt
		point.Spot = &spot
		point.Data = rows
		points = append(points, point)
	}
	return points, nil
}

func (s *SQLite) chainSnapshotAtOrBefore(symbol, expiration string, targetAt time.Time) (string, time.Time, bool, error) {
	target := storedTimestamp(targetAt)
	oldest := storedTimestamp(targetAt.Add(-chainHistorySnapshotTolerance))
	var fetched string
	err := s.db.QueryRow(`SELECT fetched_at FROM option_snapshots
WHERE symbol=? AND expiration=?
  AND julianday(fetched_at) <= julianday(?)
  AND julianday(fetched_at) >= julianday(?)
ORDER BY julianday(fetched_at) DESC, fetched_at DESC LIMIT 1`, symbol, expiration, target, oldest).Scan(&fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	when, err := parseStoredTime(fetched)
	if err != nil {
		return "", time.Time{}, false, err
	}
	return fetched, when, true, nil
}

func (s *SQLite) quoteAtOrBefore(symbol, chainFetched string, chainAt time.Time) (float64, time.Time, bool, error) {
	latest := chainFetched
	oldest := storedTimestamp(chainAt.Add(-chainHistorySnapshotTolerance))
	var fetched string
	var price sql.NullFloat64
	err := s.db.QueryRow(`SELECT fetched_at, price FROM quote_snapshots
WHERE symbol=? AND price > 0
  AND julianday(fetched_at) <= julianday(?)
  AND julianday(fetched_at) >= julianday(?)
ORDER BY julianday(fetched_at) DESC, fetched_at DESC LIMIT 1`, symbol, latest, oldest).Scan(&fetched, &price)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !price.Valid) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	when, err := parseStoredTime(fetched)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return price.Float64, when, true, nil
}

func (s *SQLite) chainRowsAt(symbol, expiration, fetched string) ([]model.OptionRow, error) {
	rows, err := s.db.Query(`SELECT current.contract_symbol, current.contract_type, current.strike,
 current.last_price, current.bid, current.ask, current.volume, current.open_interest,
 current.implied_volatility, current.gamma, current.in_the_money, current.change_percent,
 current.provider,
 (SELECT previous.open_interest FROM option_snapshots previous
   WHERE previous.symbol=current.symbol AND previous.expiration=current.expiration
     AND previous.contract_symbol=current.contract_symbol AND previous.open_interest > 0
     AND julianday(previous.fetched_at) < julianday(current.fetched_at)
   ORDER BY julianday(previous.fetched_at) DESC, previous.fetched_at DESC LIMIT 1) AS fallback_oi
FROM option_snapshots current
WHERE current.symbol=? AND current.expiration=? AND current.fetched_at=?
ORDER BY current.strike, current.contract_type`, symbol, expiration, fetched)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := make([]model.OptionRow, 0)
	for rows.Next() {
		var row model.OptionRow
		var strike, last, bid, ask, iv, gammaValue, change, fallbackOI sql.NullFloat64
		var volume, oi sql.NullInt64
		var providerName sql.NullString
		var itm sql.NullInt64
		if err := rows.Scan(&row.ContractSymbol, &row.ContractType, &strike, &last, &bid, &ask,
			&volume, &oi, &iv, &gammaValue, &itm, &change, &providerName, &fallbackOI); err != nil {
			return nil, err
		}
		row.Expiration = expiration
		row.Strike, row.LastPrice, row.Bid, row.Ask = nullableFloat(strike), nullableFloat(last), nullableFloat(bid), nullableFloat(ask)
		row.Volume, row.OpenInterest = nullableInt(volume), nullableInt(oi)
		row.ImpliedVolatility, row.Gamma, row.ChangePercent = nullableFloat(iv), nullableFloat(gammaValue), nullableFloat(change)
		row.Provider = providerName.String
		row.InTheMoney = itm.Valid && itm.Int64 != 0
		row.InTheMoneyKnown = itm.Valid
		if (row.OpenInterest == nil || *row.OpenInterest <= 0) && fallbackOI.Valid && fallbackOI.Float64 > 0 {
			fallback := int64(fallbackOI.Float64)
			row.OpenInterest = &fallback
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
