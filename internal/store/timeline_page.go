package store

// timelineEpochExpr is the merged timeline's sort key for timestamped text
// columns: Unix milliseconds with sub-second precision, matching Go's
// time.Time.UnixMilli for the same instant. It is used in both ORDER BY and
// keyset predicates, so the expression indexes added in schema version 10
// (idx_*_timeline) stay usable.
//
// unixepoch(..., 'subsec') returns a float; casting to INTEGER truncates
// toward zero, which is what UnixMilli does for post-1970 timestamps.
const timelineEpochExpr = "CAST(unixepoch(ts, 'subsec') * 1000 AS INTEGER)"
