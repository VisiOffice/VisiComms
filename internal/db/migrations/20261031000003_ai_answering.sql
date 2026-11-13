-- The AI answers calls through OpenAI. A call it picked up carries OpenAI's
-- call ID, which also stops a repeated webhook from answering it twice.
ALTER TABLE calls ADD COLUMN ai_call_id text UNIQUE;
