import logging
import os
import sqlite3
import tempfile
from typing import Optional, Dict, Any, Union
import warnings
import wave
from contextlib import closing

_DEFAULT_OPENAI_BASE_URL = "https://api.openai.com/v1"


def _str_to_bool(value: Optional[Union[str, bool]]) -> bool:
    if isinstance(value, bool):
        return value
    if value is None:
        return False
    return str(value).strip().lower() in {"1", "true", "yes", "on"}


try:
    # TODO: 重写读取{Users}/.chatlog/whisper.json
    pass
except Exception:
    LOCAL_WHISPER = _str_to_bool(
        os.getenv("CHATLOG_LOCAL_WHISPER", os.getenv("LOCAL_WHISPER"))
    )
    OPENAI_API_KEY = os.getenv("OPENAI_API_KEY", "")
    OPENAI_BASE_URL = os.getenv(
        "OPENAI_BASE_URL",
        os.getenv("CHATLOG_OPENAI_BASE_URL", _DEFAULT_OPENAI_BASE_URL),
    )
else:
    """
    LOCAL_WHISPER = _str_to_bool(getattr(_wechatlog_config, "LOCAL_WHISPER", False))
    OPENAI_API_KEY = getattr(_wechatlog_config, "OPENAI_API_KEY", "")
    OPENAI_BASE_URL = (
        getattr(_wechatlog_config, "OPENAI_BASE_URL", _DEFAULT_OPENAI_BASE_URL)
        or _DEFAULT_OPENAI_BASE_URL
    )
    """  #! TODO

# --- Simplified chunking configuration: only split every 10 seconds ---
WHISPER_CHUNK_SECONDS = 10

# 条件导入
if LOCAL_WHISPER:
    try:
        import whisper

        LOCAL_WHISPER_AVAILABLE = True
    except ImportError:
        LOCAL_WHISPER_AVAILABLE = False
        warnings.warn("whisper package not available, falling back to OpenAI API")
else:
    LOCAL_WHISPER_AVAILABLE = False

if not LOCAL_WHISPER or not LOCAL_WHISPER_AVAILABLE:
    try:
        from openai import OpenAI

        OPENAI_AVAILABLE = True
    except ImportError:
        OPENAI_AVAILABLE = False
        warnings.warn("openai package not available")

# Import silk2audio from PyWxDump
try:
    import sys

    sys.path.append("./PyWxDump")
    # TODO: _pywxdump_utils = importlib.import_module("pywxdump.db.utils.common_utils")
    # TODO: silk2audio = getattr(_pywxdump_utils, "silk2audio")

    SILK_AVAILABLE = True
except Exception:
    SILK_AVAILABLE = False
    warnings.warn(
        "silk2audio from PyWxDump not available, cannot decode silk audio from database"
    )


class TranscriptionService:
    """Service for transcribing audio messages to text"""

    def __init__(self, db_path: str):
        self.db_path = db_path
        self._openai_client = None
        self._whisper_model = None

        # Initialize appropriate client/model
        if LOCAL_WHISPER and LOCAL_WHISPER_AVAILABLE:
            self._init_local_whisper()
        elif OPENAI_AVAILABLE:
            self._init_openai_client()
        else:
            raise RuntimeError("No transcription backend available")

    def _get_silk_audio_from_db(self, msg_svr_id: str) -> Optional[bytes]:
        """
        Get silk audio data from database using MSG and Media tables

        Args:
            msg_svr_id: Message server ID

        Returns:
            Silk audio data bytes or None if not found
        """
        try:
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()

            # First, verify this is a voice message in MSG table (Type = 34)
            cursor.execute(
                """
                SELECT MsgSvrID FROM MSG WHERE MsgSvrID = ? AND Type = 34
            """,
                (msg_svr_id,),
            )

            msg_result = cursor.fetchone()
            if not msg_result:
                logging.warning(
                    f"Message {msg_svr_id} not found in MSG table or not a voice message (Type != 34)"
                )
                conn.close()
                return None

            # Then get the silk audio data from Media table using Reserved0 field
            cursor.execute(
                """
                SELECT Buf FROM Media WHERE Reserved0 = ?
            """,
                (msg_svr_id,),
            )

            result = cursor.fetchone()
            conn.close()

            if result and result[0]:
                logging.info(
                    f"Found silk audio data for message {msg_svr_id}: {len(result[0])} bytes"
                )
                return result[0]
            else:
                logging.warning(
                    f"No silk audio data found in Media.Reserved0 for message {msg_svr_id}"
                )
                return None

        except Exception as e:
            logging.error(f"Error getting silk audio from database: {e}")
            return None

    def _convert_silk_to_wav(self, silk_data: bytes) -> Optional[str]:
        """Convert silk audio data to wav file and return temp file path"""
        if not SILK_AVAILABLE:
            logging.error("silk2audio from PyWxDump not available for audio conversion")
            return None

        try:
            import tempfile

            # Create temp wav file
            wav_temp_path = tempfile.mktemp(suffix=".wav")

            # Use PyWxDump's silk2audio function to convert silk to wav
            # TODO: wav_data = silk2audio(silk_data, is_wave=True, save_path=wav_temp_path, rate=24000)

            if os.path.exists(wav_temp_path) and os.path.getsize(wav_temp_path) > 0:
                logging.info(f"Successfully converted silk to wav: {wav_temp_path}")
                return wav_temp_path
            else:
                logging.error(
                    "Failed to convert silk to wav - output file empty or missing"
                )
                return None

        except Exception as e:
            logging.error(
                f"Error converting silk to wav using PyWxDump's silk2audio: {e}"
            )
            return None

    def _init_local_whisper(self):
        """Initialize local Whisper model with CUDA support and large model"""
        if not LOCAL_WHISPER:
            logging.info("Local Whisper disabled via configuration")
            return

        if not LOCAL_WHISPER_AVAILABLE:
            logging.error("Local Whisper requested but whisper package unavailable")
            self._handle_whisper_init_failure()
            return

        try:
            import torch  # type: ignore
        except ImportError:
            torch = None

        device = "cuda" if torch and torch.cuda.is_available() else "cpu"
        model_name = os.getenv(
            "CHATLOG_WHISPER_MODEL",
            os.getenv("WHISPER_MODEL", "large-v2" if device == "cuda" else "base"),
        )

        try:
            self._whisper_model = whisper.load_model(model_name, device=device)
            self._whisper_device = device
            logging.info(
                f"Local Whisper model '{model_name}' loaded successfully on {device}"
            )
        except Exception as exc:
            logging.error(f"Failed to load Whisper model '{model_name}': {exc}")
            self._handle_whisper_init_failure()

    def _handle_whisper_init_failure(self):
        """Handle Whisper initialization failure"""
        # Fall back to OpenAI if available
        if OPENAI_AVAILABLE:
            logging.info("Falling back to OpenAI Whisper API")
            self._init_openai_client()
        else:
            raise RuntimeError(
                "Failed to initialize both local Whisper and OpenAI Whisper API"
            )

    def _init_openai_client(self):
        """Initialize OpenAI client"""
        try:
            self._openai_client = OpenAI(
                api_key=OPENAI_API_KEY, base_url=OPENAI_BASE_URL
            )
            logging.info("OpenAI client initialized successfully")
        except Exception as e:
            logging.error(f"Failed to initialize OpenAI client: {e}")
            raise

    def get_existing_transcription(self, msg_svr_id: str) -> Optional[str]:
        """
        Get existing transcription from database using MSG table

        Args:
            msg_svr_id: Message server ID

        Returns:
            Existing transcription text or None if not found
        """
        try:
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()

            # First check MSG table to verify this is a voice message (Type = 34)
            cursor.execute(
                """
                SELECT MsgSvrID, StrContent FROM MSG
                WHERE MsgSvrID = ? AND Type = 34
            """,
                (msg_svr_id,),
            )

            msg_result = cursor.fetchone()
            if msg_result:
                # Try to extract any existing transcription from StrContent
                str_content = msg_result[1]
                if str_content:
                    # MSG table for voice messages typically contains XML like:
                    # <msg><voicemsg voiceformat="4" length="2732" endflag="1" cancelflag="0" voicelength="1839" fromusername="xxx" isPlayed="0" /></msg>
                    # Check if there's any transcription data in the XML
                    try:
                        import xml.etree.ElementTree as ET

                        root = ET.fromstring(str_content)
                        voicemsg = root.find("voicemsg")
                        if voicemsg is not None:
                            # Look for any transcription attributes
                            transcription = voicemsg.get("transcription")
                            if transcription:
                                conn.close()
                                return transcription
                    except (ET.ParseError, AttributeError):
                        pass

            # Check if we have stored transcription in our separate table
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS WL_TRANSCRIPTIONS (
                    MsgSvrID TEXT PRIMARY KEY,
                    transcription TEXT NOT NULL,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                )
            """
            )

            cursor.execute(
                """
                SELECT transcription FROM WL_TRANSCRIPTIONS WHERE MsgSvrID = ?
            """,
                (msg_svr_id,),
            )

            transcription_result = cursor.fetchone()
            conn.close()

            if transcription_result:
                return transcription_result[0]

        except Exception as e:
            logging.error(f"Error getting existing transcription: {e}")

        return None

    def store_transcription(self, msg_svr_id: str, transcription: str):
        """
        Store transcription in database

        Args:
            msg_svr_id: Message server ID
            transcription: Transcription text
        """
        try:
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()

            # Store in WL_TRANSCRIPTIONS table
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS WL_TRANSCRIPTIONS (
                    MsgSvrID TEXT PRIMARY KEY,
                    transcription TEXT NOT NULL,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                )
            """
            )

            # Verify table was created
            cursor.execute(
                """
                SELECT name FROM sqlite_master WHERE type='table' AND name='WL_TRANSCRIPTIONS'
            """
            )
            if not cursor.fetchone():
                raise RuntimeError("Failed to create WL_TRANSCRIPTIONS table")

            cursor.execute(
                """
                INSERT OR REPLACE INTO WL_TRANSCRIPTIONS (MsgSvrID, transcription)
                VALUES (?, ?)
            """,
                (msg_svr_id, transcription),
            )

            # Verify insertion
            cursor.execute(
                """
                SELECT transcription FROM WL_TRANSCRIPTIONS WHERE MsgSvrID = ?
            """,
                (msg_svr_id,),
            )
            inserted_record = cursor.fetchone()
            if not inserted_record:
                raise RuntimeError("Failed to insert transcription record")

            # Also update WL_MSG table if record exists
            cursor.execute(
                """
                SELECT name FROM sqlite_master WHERE type='table' AND name='WL_MSG'
            """
            )
            if cursor.fetchone():
                cursor.execute(
                    """
                    UPDATE WL_MSG SET textualized_content = ?
                    WHERE MsgSvrID = ? AND type_name = '语音' AND textualized_content IS NULL
                """,
                    (transcription, msg_svr_id),
                )

            conn.commit()
            conn.close()
            logging.info(f"Stored transcription for message {msg_svr_id}")

        except Exception as e:
            logging.error(f"Error storing transcription: {e}")
            if "conn" in locals():
                conn.rollback()
                conn.close()
            raise

    def transcribe_audio_file(self, audio_path: str) -> Optional[str]:
        """
        Transcribe audio file using available backend

        Args:
            audio_path: Path to audio file

        Returns:
            Transcription text or None if failed
        """
        if not os.path.exists(audio_path):
            logging.error(f"Audio file not found: {audio_path}")
            return None

        try:
            if LOCAL_WHISPER and self._whisper_model:
                return self._transcribe_with_local_whisper(audio_path)
            elif self._openai_client:
                return self._transcribe_with_openai(audio_path)
            else:
                logging.error("No transcription backend available")
                return None

        except Exception as e:
            logging.error(f"Error transcribing audio: {e}")
            return None

    def _transcribe_with_local_whisper(self, audio_path: str) -> Optional[str]:
        """Transcribe using local Whisper model with optimized settings"""
        try:
            if self._whisper_model is not None:
                # Use Chinese language hint for better accuracy
                result = self._whisper_model.transcribe(audio_path, language="zh")
                # Handle type safely
                if isinstance(result, dict) and "text" in result:
                    return str(result["text"]).strip()
            return None
        except Exception as e:
            logging.error(f"Local Whisper transcription failed: {e}")
            return None

    def _transcribe_with_openai(self, audio_path: str) -> Optional[str]:
        """Transcribe using OpenAI Whisper API (chunk every 10s if duration > 10s)."""
        try:
            if self._openai_client is None:
                return None
            is_wav = audio_path.lower().endswith(".wav")
            if is_wav:
                try:
                    with closing(wave.open(audio_path, "rb")) as wf:
                        framerate = wf.getframerate()
                        nframes = wf.getnframes()
                        duration_sec = nframes / float(framerate) if framerate else 0
                    if duration_sec > WHISPER_CHUNK_SECONDS:
                        logging.info(
                            f"Duration {duration_sec:.2f}s > {WHISPER_CHUNK_SECONDS}s: chunking"
                        )
                        return self._chunk_wav_and_transcribe(
                            audio_path, max_chunk_seconds=WHISPER_CHUNK_SECONDS
                        )
                except Exception as inspect_err:
                    logging.debug(
                        f"Duration inspect failed, falling back to single-shot: {inspect_err}"
                    )
            with open(audio_path, "rb") as audio_file:
                transcription = self._openai_client.audio.transcriptions.create(
                    model="whisper-1", file=audio_file, language="zh"
                )
            return (
                transcription.text.strip()
                if getattr(transcription, "text", None)
                else None
            )
        except Exception as e:
            logging.error(f"OpenAI Whisper transcription failed: {e}")
            # Fallback: attempt chunking anyway if wav
            try:
                if audio_path.lower().endswith(".wav"):
                    logging.info("Retry after failure: forcing 10s chunk transcription")
                    return self._chunk_wav_and_transcribe(
                        audio_path, max_chunk_seconds=WHISPER_CHUNK_SECONDS
                    )
            except Exception as chunk_err:
                logging.error(f"Forced chunk transcription failed: {chunk_err}")
            return None

    def _chunk_wav_and_transcribe(
        self,
        audio_path: str,
        max_chunk_seconds: int = WHISPER_CHUNK_SECONDS,
        target_max_bytes: Optional[int] = None,
    ) -> Optional[str]:
        """Chunk a WAV file every fixed number of seconds (default 10s) and transcribe sequentially."""
        if self._openai_client is None:
            return None
        if not audio_path.lower().endswith(".wav"):
            # As a safeguard just do single-shot
            try:
                with open(audio_path, "rb") as f:
                    resp = self._openai_client.audio.transcriptions.create(
                        model="whisper-1", file=f, language="zh"
                    )
                return resp.text.strip() if getattr(resp, "text", None) else None
            except Exception as e:
                logging.error(f"Single-shot (non-wav) transcription failed: {e}")
                return None

        try:
            with closing(wave.open(audio_path, "rb")) as wf:
                params = {
                    "nchannels": wf.getnchannels(),
                    "sampwidth": wf.getsampwidth(),
                    "framerate": wf.getframerate(),
                    "nframes": wf.getnframes(),
                }
                bytes_per_frame = params["sampwidth"] * params["nchannels"]
                if bytes_per_frame == 0:
                    logging.error("Invalid WAV parameters (bytes_per_frame=0)")
                    return None
                frames_per_chunk = (
                    int(max_chunk_seconds * params["framerate"])
                    if params["framerate"]
                    else 0
                )
                if frames_per_chunk <= 0:
                    logging.error("Computed frames_per_chunk <= 0")
                    return None

                chunks_text = []
                total_frames = params["nframes"]
                processed = 0
                idx = 0
                while processed < total_frames:
                    frames_to_read = min(frames_per_chunk, total_frames - processed)
                    audio_bytes = wf.readframes(frames_to_read)
                    processed += frames_to_read
                    idx += 1

                    # Write temp wav chunk
                    tmp_path = tempfile.mktemp(suffix=f"_chunk{idx}.wav")
                    try:
                        with closing(wave.open(tmp_path, "wb")) as out_wav:
                            out_wav.setnchannels(params["nchannels"])
                            out_wav.setsampwidth(params["sampwidth"])
                            out_wav.setframerate(params["framerate"])
                            out_wav.writeframes(audio_bytes)

                        with open(tmp_path, "rb") as chunk_f:
                            resp = self._openai_client.audio.transcriptions.create(
                                model="whisper-1", file=chunk_f, language="zh"
                            )
                        chunk_text = (
                            resp.text.strip() if getattr(resp, "text", None) else ""
                        )
                        logging.info(
                            f"Transcribed chunk {idx}: frames={frames_to_read}, text_len={len(chunk_text)}"
                        )
                        if chunk_text:
                            chunks_text.append(chunk_text)
                    except Exception as ce:
                        logging.error(f"Failed to transcribe chunk {idx}: {ce}")
                    finally:
                        try:
                            if os.path.exists(tmp_path):
                                os.unlink(tmp_path)
                        except Exception:
                            pass

                combined = "\n".join(chunks_text).strip()
                return combined if combined else None
        except Exception as e:
            logging.error(f"Error during chunked wav transcription: {e}")
            return None

    def transcribe_message(
        self, msg_svr_id: str, audio_path: Optional[str] = None, force: bool = False
    ) -> Dict[str, Any]:
        """
        Transcribe a voice message

        Args:
            msg_svr_id: Message server ID
            audio_path: Optional audio file path

        Returns:
            Dictionary with success status, transcription text, and source info
        """
        # First, check for existing transcription unless force regenerate
        if not force:
            existing_transcription = self.get_existing_transcription(msg_svr_id)
            if existing_transcription:
                return {
                    "success": True,
                    "transcription": existing_transcription,
                    "source": "cached",
                    "backend": None,
                    "force": False,
                    "message": "Using existing transcription",
                }

        # If audio_path not provided, try to get it from database first (legacy files)
        if not audio_path:
            audio_path = self._get_audio_path_from_db(msg_svr_id)

        # If no file path found or file doesn't exist, try to get silk data from database
        if not audio_path or not os.path.exists(audio_path):
            logging.info(
                f"Audio file not found, attempting to get silk data from database for message {msg_svr_id}"
            )

            silk_data = self._get_silk_audio_from_db(msg_svr_id)
            if silk_data:
                # Convert silk to wav
                audio_path = self._convert_silk_to_wav(silk_data)
                if not audio_path:
                    return {
                        "success": False,
                        "message": "Failed to convert silk audio data to wav format",
                    }
            else:
                return {
                    "success": False,
                    "message": "No audio data found in database (neither file path nor silk data)",
                }

        # Now we should have a valid audio path
        if not audio_path or not os.path.exists(audio_path):
            return {"success": False, "message": "Audio file processing failed"}

        # Transcribe the audio
        transcription = self.transcribe_audio_file(audio_path)

        # Clean up temporary file if we created one from silk data
        if audio_path and audio_path.startswith(tempfile.gettempdir()):
            try:
                os.unlink(audio_path)
                logging.info(f"Cleaned up temporary audio file: {audio_path}")
            except Exception as e:
                logging.warning(f"Failed to clean up temporary file {audio_path}: {e}")
        if not transcription:
            backend = (
                "local_whisper"
                if (LOCAL_WHISPER and LOCAL_WHISPER_AVAILABLE)
                else "openai_whisper"
            )
            return {
                "success": False,
                "backend": backend,
                "force": force,
                "message": f"Transcription failed using {backend}",
            }

        # Store the transcription
        self.store_transcription(msg_svr_id, transcription)

        backend = (
            "local_whisper"
            if (LOCAL_WHISPER and self._whisper_model)
            else "openai_whisper"
        )

        return {
            "success": True,
            "transcription": transcription,
            "source": "generated",
            "backend": backend,
            "force": force,
            "message": f"Transcription completed using {backend}{' (regenerated)' if force else ''}",
        }

    def _delete_existing_transcription(self, msg_svr_id: str) -> bool:
        """Delete existing transcription so it can be regenerated."""
        try:
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS WL_TRANSCRIPTIONS (
                    MsgSvrID TEXT PRIMARY KEY,
                    transcription TEXT NOT NULL,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
                )
                """
            )
            cursor.execute(
                "DELETE FROM WL_TRANSCRIPTIONS WHERE MsgSvrID = ?", (msg_svr_id,)
            )
            # Clear WL_MSG textualized_content if table exists
            cursor.execute(
                "SELECT name FROM sqlite_master WHERE type='table' AND name='WL_MSG'"
            )
            if cursor.fetchone():
                cursor.execute(
                    """
                    UPDATE WL_MSG SET textualized_content = NULL
                    WHERE MsgSvrID = ? AND type_name = '语音'
                    """,
                    (msg_svr_id,),
                )
            conn.commit()
            conn.close()
            return True
        except Exception as e:
            logging.error(
                f"Error deleting existing transcription for {msg_svr_id}: {e}"
            )
            try:
                if "conn" in locals():
                    conn.rollback()
                    conn.close()
            except Exception:
                pass
            return False

    def regenerate_transcription(
        self, msg_svr_id: str, audio_path: Optional[str] = None
    ) -> Dict[str, Any]:
        """Force regeneration: delete existing then transcribe with force=True."""
        deleted = self._delete_existing_transcription(msg_svr_id)
        result = self.transcribe_message(msg_svr_id, audio_path=audio_path, force=True)
        result["regenerated"] = True
        result["deleted_previous"] = deleted
        return result

    def _get_audio_path_from_db(self, msg_svr_id: str) -> Optional[str]:
        """
        Get audio file path from MSG table (legacy support)

        Note: This method is mainly for legacy compatibility.
        The new approach uses silk data directly from Media table.

        Args:
            msg_svr_id: Message server ID

        Returns:
            Audio file path or None if not found
        """
        try:
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()

            # Check MSG table for voice messages (Type = 34)
            cursor.execute(
                """
                SELECT MsgSvrID, StrContent FROM MSG 
                WHERE MsgSvrID = ? AND Type = 34
            """,
                (msg_svr_id,),
            )

            result = cursor.fetchone()
            conn.close()

            if result:
                # MSG table typically doesn't contain file paths for voice messages
                # Voice messages are stored as silk data in Media table
                # This method mainly exists for legacy compatibility
                logging.info(
                    f"Voice message {msg_svr_id} found in MSG table, but audio path not available from this table"
                )
                return None
            else:
                logging.warning(f"Voice message {msg_svr_id} not found in MSG table")
                return None

        except Exception as e:
            logging.error(f"Error getting audio path from database: {e}")
            return None
