-- Earlier panel versions registered the local host with the sentinel
-- scheme "local://" and dispatched on it in-process. The local node is now
-- reached through the daemon's unix socket like any other node; this UPDATE
-- rewrites the address on first boot of the new build.
UPDATE nodes SET address = 'unix:///var/run/nft-forward.sock'
WHERE address = 'local://';
