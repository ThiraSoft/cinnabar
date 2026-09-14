-- Filtre sur les metadata d'un message, pour la recherche et le listage.
--
-- Le filtre arrive déjà validé par memory.MetadataFilter.Validate: la
-- fonction ne revérifie pas la grammaire, elle rend faux sur ce qu'elle ne
-- reconnaît pas. Elle est évaluée ligne par ligne dans les stratégies, ce
-- qui garde leurs requêtes constantes au lieu de composer du SQL à partir
-- d'une entrée du client.
--
-- Une clé absente rend toute feuille fausse, sauf "ne" et "exists": false.
-- Les comparaisons d'ordre ne portent que sur deux nombres ou deux chaînes,
-- les chaînes en collation "C" pour qu'un même filtre rende la même chose
-- quelle que soit la locale de la base.
CREATE FUNCTION cinnabar_metadata_match(meta jsonb, f jsonb)
RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
	child jsonb;
	k     text;
	op    text;
	v     jsonb;
	x     jsonb;
	t     text;
BEGIN
	IF f IS NULL THEN
		RETURN TRUE;
	END IF;

	IF f ? 'all' THEN
		FOR child IN SELECT jsonb_array_elements(f->'all') LOOP
			IF NOT cinnabar_metadata_match(meta, child) THEN
				RETURN FALSE;
			END IF;
		END LOOP;
		RETURN TRUE;
	END IF;

	IF f ? 'any' THEN
		FOR child IN SELECT jsonb_array_elements(f->'any') LOOP
			IF cinnabar_metadata_match(meta, child) THEN
				RETURN TRUE;
			END IF;
		END LOOP;
		RETURN FALSE;
	END IF;

	k  := f->>'key';
	op := f->>'op';
	v  := f->'value';

	IF meta IS NULL OR jsonb_typeof(meta) <> 'object' OR NOT meta ? k THEN
		RETURN op = 'ne' OR (op = 'exists' AND v = 'false'::jsonb);
	END IF;
	x := meta->k;

	CASE op
		WHEN 'eq' THEN
			RETURN x = v;
		WHEN 'ne' THEN
			RETURN x <> v;
		WHEN 'in' THEN
			RETURN EXISTS (SELECT 1 FROM jsonb_array_elements(v) AS e(val) WHERE e.val = x);
		WHEN 'exists' THEN
			RETURN v = 'true'::jsonb;
		WHEN 'lt', 'lte', 'gt', 'gte' THEN
			t := jsonb_typeof(x);
			IF t <> jsonb_typeof(v) THEN
				RETURN FALSE;
			END IF;
			IF t = 'number' THEN
				RETURN CASE op
					WHEN 'lt'  THEN (x #>> '{}')::numeric <  (v #>> '{}')::numeric
					WHEN 'lte' THEN (x #>> '{}')::numeric <= (v #>> '{}')::numeric
					WHEN 'gt'  THEN (x #>> '{}')::numeric >  (v #>> '{}')::numeric
					ELSE            (x #>> '{}')::numeric >= (v #>> '{}')::numeric
				END;
			END IF;
			IF t = 'string' THEN
				RETURN CASE op
					WHEN 'lt'  THEN (x #>> '{}') COLLATE "C" <  (v #>> '{}') COLLATE "C"
					WHEN 'lte' THEN (x #>> '{}') COLLATE "C" <= (v #>> '{}') COLLATE "C"
					WHEN 'gt'  THEN (x #>> '{}') COLLATE "C" >  (v #>> '{}') COLLATE "C"
					ELSE            (x #>> '{}') COLLATE "C" >= (v #>> '{}') COLLATE "C"
				END;
			END IF;
			RETURN FALSE;
		ELSE
			RETURN FALSE;
	END CASE;
END
$$;
