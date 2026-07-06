-- Generic key/value store for UI-editable operator settings (first key: ssh_addr).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
