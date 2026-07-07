-- Backfill the superseded status. Before it existed, a blue-green cutover left
-- the replaced deployments sitting at "running" forever, so an app accrued many
-- "running" rows. Retire all but the newest running row per app; that newest
-- one is the deployment actually holding traffic.
UPDATE deployments SET status = 'superseded'
WHERE status = 'running'
  AND id NOT IN (
      SELECT MAX(id) FROM deployments WHERE status = 'running' GROUP BY app_id
  );
