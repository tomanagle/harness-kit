ALTER TABLE users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;
CREATE TABLE account_challenges(hash text PRIMARY KEY,user_id text NOT NULL REFERENCES users(id),purpose text NOT NULL CHECK(purpose IN ('verify','reset')),expires_at timestamptz NOT NULL,created_at timestamptz NOT NULL DEFAULT now(),used boolean NOT NULL DEFAULT false);
CREATE TABLE mail_jobs(id text PRIMARY KEY,recipient text NOT NULL,subject text NOT NULL,body text NOT NULL,attempts integer NOT NULL DEFAULT 0,available_at timestamptz NOT NULL DEFAULT now(),expires_at timestamptz NOT NULL);
CREATE INDEX mail_jobs_available ON mail_jobs(available_at);
