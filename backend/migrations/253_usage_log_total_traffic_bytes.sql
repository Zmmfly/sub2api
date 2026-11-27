-- Count measured HTTP entity-body bytes across both links. A row without any
-- actual byte measurements remains unknown, including logs from older versions.
CREATE FUNCTION usage_log_total_traffic_bytes(jsonb) RETURNS bigint
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT CASE
        WHEN $1 IS NULL THEN NULL
        WHEN COALESCE(
            $1 #>> '{upstream,request,actual_bytes}',
            $1 #>> '{upstream,response,actual_bytes}',
            $1 #>> '{downstream,request,actual_bytes}',
            $1 #>> '{downstream,response,actual_bytes}'
        ) IS NOT NULL THEN
            COALESCE(($1 #>> '{upstream,request,actual_bytes}')::bigint, 0) +
            COALESCE(($1 #>> '{upstream,response,actual_bytes}')::bigint, 0) +
            COALESCE(($1 #>> '{downstream,request,actual_bytes}')::bigint, 0) +
            COALESCE(($1 #>> '{downstream,response,actual_bytes}')::bigint, 0)
    END;
$$;
