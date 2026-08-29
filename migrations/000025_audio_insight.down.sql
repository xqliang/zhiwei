DROP TABLE IF EXISTS speaker_session_state;
ALTER TABLE transcript DROP COLUMN overall_mood;
ALTER TABLE transcript DROP COLUMN weather_cues;
ALTER TABLE transcript DROP COLUMN background_sounds;
ALTER TABLE transcript DROP COLUMN acoustic_scene;
