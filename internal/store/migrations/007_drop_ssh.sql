-- git is served over HTTP only: SSH keys and the SSH host key are gone.
DROP TABLE ssh_keys;
DELETE FROM server_secrets WHERE name = 'ssh_host_ed25519';
