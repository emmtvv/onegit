-- Colour theme of the web UI; 'system' follows the browser's preference.
ALTER TABLE users ADD COLUMN theme TEXT NOT NULL DEFAULT 'system' CHECK (theme IN ('system', 'light', 'dark'));
