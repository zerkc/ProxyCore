-- A PRIMARY can unlink one enrolled node. unlinked_at marks a revocation that
-- was an explicit unlink (as opposed to a demote/promote retirement) so sync
-- endpoints can answer the node with an authoritative NODE_UNLINKED signal.
-- Re-enrolling the same node clears it.
ALTER TABLE enrolled_nodes ADD COLUMN unlinked_at TEXT COLLATE BINARY CHECK (unlinked_at IS NULL OR (instr(unlinked_at,char(0)) = 0 AND (
    unlinked_at IN ('0','2') OR (length(unlinked_at) = 31 AND substr(unlinked_at,1,1) = '1'
    AND substr(unlinked_at,22,1) = '.' AND substr(unlinked_at,2,20) NOT GLOB '*[^0-9]*'
    AND substr(unlinked_at,23,9) NOT GLOB '*[^0-9]*' AND substr(unlinked_at,2,20) <= '18446744073709551615'))));
