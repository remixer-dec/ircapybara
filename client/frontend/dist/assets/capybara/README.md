# Capybara avatar assets

All SVGs in this directory are discovered by the Go/Wails bridge. User avatars choose a style by nickname hash and apply a deterministic nickname-seeded palette so every user gets a stable but varied avatar. The app brand mark is always `capybara-1.svg` with its original colors.

They are deliberately isolated here so new artwork can be added without changing application code.
