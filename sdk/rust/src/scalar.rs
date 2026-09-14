use crate::ClientError;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use std::fmt::{self, Display, Formatter};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

macro_rules! lossless_scalar {
    ($name:ident, $canonicalizer:ident) => {
        #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
        pub struct $name(String);

        impl $name {
            /// Parses and canonicalizes the scalar's lossless wire string.
            ///
            /// # Errors
            ///
            /// Returns `CLIENT_SCALAR_INVALID` when the value is outside the
            /// scalar range or is not in its accepted wire grammar.
            pub fn parse(value: impl AsRef<str>) -> Result<Self, ClientError> {
                $canonicalizer(value.as_ref()).map(Self)
            }

            #[must_use]
            pub fn as_str(&self) -> &str {
                &self.0
            }

            #[must_use]
            pub fn into_string(self) -> String {
                self.0
            }
        }

        impl Display for $name {
            fn fmt(&self, formatter: &mut Formatter<'_>) -> fmt::Result {
                formatter.write_str(&self.0)
            }
        }

        impl Serialize for $name {
            fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
            where
                S: Serializer,
            {
                serializer.serialize_str(&self.0)
            }
        }

        impl<'de> Deserialize<'de> for $name {
            fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
            where
                D: Deserializer<'de>,
            {
                let value = String::deserialize(deserializer)?;
                Self::parse(value).map_err(serde::de::Error::custom)
            }
        }
    };
}

lossless_scalar!(Int64, canonical_i64);
lossless_scalar!(UInt64, canonical_u64);
lossless_scalar!(BigInt, canonical_bigint);
lossless_scalar!(Decimal, canonical_decimal);
lossless_scalar!(Timestamp, canonical_timestamp);
lossless_scalar!(Duration, canonical_i64);
lossless_scalar!(Uuid, canonical_uuid);
lossless_scalar!(Bytes, canonical_bytes);

fn invalid_scalar() -> ClientError {
    ClientError::new("CLIENT_SCALAR_INVALID", "canonical scalar value is invalid")
}

fn canonical_i64(value: &str) -> Result<String, ClientError> {
    if !integer_grammar(value) {
        return Err(invalid_scalar());
    }
    value
        .parse::<i64>()
        .map(|number| number.to_string())
        .map_err(|_| invalid_scalar())
}

fn canonical_u64(value: &str) -> Result<String, ClientError> {
    if value.is_empty() || !value.bytes().all(|byte| byte.is_ascii_digit()) {
        return Err(invalid_scalar());
    }
    value
        .parse::<u64>()
        .map(|number| number.to_string())
        .map_err(|_| invalid_scalar())
}

fn canonical_bigint(value: &str) -> Result<String, ClientError> {
    if !integer_grammar(value) {
        return Err(invalid_scalar());
    }
    let negative = value.starts_with('-');
    let digits = value.trim_start_matches('-').trim_start_matches('0');
    if digits.is_empty() {
        return Ok("0".to_owned());
    }
    Ok(if negative {
        format!("-{digits}")
    } else {
        digits.to_owned()
    })
}

fn canonical_decimal(value: &str) -> Result<String, ClientError> {
    let unsigned = value.strip_prefix('-').unwrap_or(value);
    let mut components = unsigned.split('.');
    let integer = components.next().unwrap_or_default();
    let fraction = components.next();
    if components.next().is_some()
        || integer.is_empty()
        || !integer.bytes().all(|byte| byte.is_ascii_digit())
        || fraction.is_some_and(|digits| {
            digits.is_empty() || !digits.bytes().all(|byte| byte.is_ascii_digit())
        })
    {
        return Err(invalid_scalar());
    }
    let integer = integer.trim_start_matches('0');
    let integer = if integer.is_empty() { "0" } else { integer };
    let fraction = fraction.unwrap_or_default().trim_end_matches('0');
    let mut canonical = if fraction.is_empty() {
        integer.to_owned()
    } else {
        format!("{integer}.{fraction}")
    };
    if value.starts_with('-') && canonical != "0" {
        canonical.insert(0, '-');
    }
    Ok(canonical)
}

fn canonical_timestamp(value: &str) -> Result<String, ClientError> {
    if value.len() < 20
        || value.as_bytes().get(4) != Some(&b'-')
        || value.as_bytes().get(7) != Some(&b'-')
        || value.as_bytes().get(10) != Some(&b'T')
        || value.as_bytes().get(13) != Some(&b':')
        || value.as_bytes().get(16) != Some(&b':')
        || value.as_bytes().get(17..19) == Some(b"60")
    {
        return Err(invalid_scalar());
    }
    let parsed = OffsetDateTime::parse(value, &Rfc3339).map_err(|_| invalid_scalar())?;
    let utc = parsed
        .checked_to_offset(time::UtcOffset::UTC)
        .ok_or_else(invalid_scalar)?;
    if !(0..=9999).contains(&utc.year()) {
        return Err(invalid_scalar());
    }
    utc.format(&Rfc3339).map_err(|_| invalid_scalar())
}

fn canonical_uuid(value: &str) -> Result<String, ClientError> {
    if value.len() != 36
        || !value.bytes().enumerate().all(|(index, byte)| match index {
            8 | 13 | 18 | 23 => byte == b'-',
            _ => byte.is_ascii_hexdigit(),
        })
    {
        return Err(invalid_scalar());
    }
    Ok(value.to_ascii_lowercase())
}

fn canonical_bytes(value: &str) -> Result<String, ClientError> {
    let decoded = URL_SAFE_NO_PAD
        .decode(value)
        .map_err(|_| invalid_scalar())?;
    Ok(URL_SAFE_NO_PAD.encode(decoded))
}

fn integer_grammar(value: &str) -> bool {
    let digits = value.strip_prefix('-').unwrap_or(value);
    !digits.is_empty() && digits.bytes().all(|byte| byte.is_ascii_digit())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalizes_lossless_scalar_spellings() {
        assert_eq!(
            BigInt::parse("-0009007199254740993").unwrap().as_str(),
            "-9007199254740993"
        );
        assert_eq!(Decimal::parse("001.2300").unwrap().as_str(), "1.23");
        assert_eq!(Decimal::parse("-0.000").unwrap().as_str(), "0");
        assert_eq!(
            Uuid::parse("550E8400-E29B-41D4-A716-446655440000")
                .unwrap()
                .as_str(),
            "550e8400-e29b-41d4-a716-446655440000"
        );
        assert_eq!(
            Timestamp::parse("2026-09-11T23:30:01.123456789+03:00")
                .unwrap()
                .as_str(),
            "2026-09-11T20:30:01.123456789Z"
        );
    }

    #[test]
    fn rejects_lossy_or_out_of_range_values() {
        assert!(Int64::parse("9223372036854775808").is_err());
        assert!(UInt64::parse("-1").is_err());
        assert!(Decimal::parse("1e2").is_err());
        assert!(Uuid::parse("550e8400e29b41d4a716446655440000").is_err());
        assert!(Bytes::parse("SGVsbG8=").is_err());
    }
}
