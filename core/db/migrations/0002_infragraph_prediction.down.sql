-- 0002 down — drop the infragraph prediction spine + cascade stats (reverse of 0002 up).
DROP TABLE IF EXISTS infragraph_cascade_stats;
DROP TABLE IF EXISTS infragraph_prediction;
DROP TYPE IF EXISTS prediction_kind;
