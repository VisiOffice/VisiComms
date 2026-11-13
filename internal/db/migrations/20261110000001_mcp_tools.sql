-- More MCP tools: claiming a thread, and proposing changes to approved info.

-- A claim is an agent's short hold on a thread while it works on a reply,
-- so two agents do not both answer. assignee_id is the agent; the claim
-- lapses at claimed_until. An assignee with no claimed_until is a lasting
-- assignment, which agents cannot take.
ALTER TABLE threads ADD COLUMN claimed_until timestamptz;

-- A draft version an agent proposed: the approved version it started from,
-- and why. Approving it is refused once a newer version has been approved,
-- so it cannot undo someone else's change.
ALTER TABLE approved_info_versions ADD COLUMN based_on integer;
ALTER TABLE approved_info_versions ADD COLUMN reason text;
