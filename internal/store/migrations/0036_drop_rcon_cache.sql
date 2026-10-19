-- The RCON plugin's own config file is the only record of its port and password.
ALTER TABLE instances DROP COLUMN rcon_password;
ALTER TABLE instances DROP COLUMN rcon_port;
