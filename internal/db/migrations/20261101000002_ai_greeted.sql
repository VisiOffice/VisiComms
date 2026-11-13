-- When the AI started speaking. Until then a call it picked up still falls
-- back to voicemail; after, the AI's conversation records it.
ALTER TABLE calls ADD COLUMN ai_greeted_at timestamptz;
