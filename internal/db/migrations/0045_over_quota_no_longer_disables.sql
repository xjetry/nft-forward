-- Reaching the global traffic quota used to be recorded by disabling the
-- account with disable_reason '流量超额'. It is now a condition of the push
-- predicate instead (a user whose usage has reached the quota gets no rules
-- pushed), so the account itself stays usable. Lift the disable from accounts
-- that carry that reason; whether they forward is decided by their quota and
-- usage from here on. Accounts disabled by an admin carry a different reason
-- and are left alone.
UPDATE users SET disabled = 0, disable_reason = '' WHERE disabled = 1 AND disable_reason = '流量超额';
