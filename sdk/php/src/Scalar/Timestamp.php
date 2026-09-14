<?php

declare(strict_types=1);

namespace Naatre\Sdk\Scalar;

use Naatre\Sdk\Attribute\WireScalar;
use Naatre\Sdk\Exception\ClientException;

#[WireScalar('Timestamp', 'rfc3339-nanosecond-string')]
final readonly class Timestamp
{
    public string $value;

    public function __construct(string $value)
    {
        $pattern = '/^(?<year>[0-9]{4})-(?<month>[0-9]{2})-(?<day>[0-9]{2})T(?<hour>[0-9]{2}):(?<minute>[0-9]{2}):(?<second>[0-9]{2})(?:\.(?<fraction>[0-9]{1,9}))?(?<zone>Z|[+-][0-9]{2}:[0-9]{2})$/';
        if (preg_match($pattern, $value, $parts) !== 1) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $year = (int) $parts['year'];
        $month = (int) $parts['month'];
        $day = (int) $parts['day'];
        $hour = (int) $parts['hour'];
        $minute = (int) $parts['minute'];
        $second = (int) $parts['second'];
        if ($month < 1 || $month > 12 || $day < 1 || $day > self::daysInMonth($year, $month)
            || $hour > 23 || $minute > 59 || $second > 59) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $zone = $parts['zone'];
        $offset = 0;
        if ($zone !== 'Z') {
            $zoneHour = (int) substr($zone, 1, 2);
            $zoneMinute = (int) substr($zone, 4, 2);
            if ($zoneHour > 23 || $zoneMinute > 59) {
                throw new ClientException('CLIENT_SCALAR_INVALID');
            }
            $offset = ($zoneHour * 60 + $zoneMinute) * ($zone[0] === '-' ? -1 : 1);
        }
        $utcMinutes = $hour * 60 + $minute - $offset;
        [$year, $month, $day, $utcMinutes] = self::normalizeMinutes($year, $month, $day, $utcMinutes);
        if ($year < 0 || $year > 9999) {
            throw new ClientException('CLIENT_SCALAR_INVALID');
        }
        $fraction = rtrim($parts['fraction'], '0');
        $this->value = sprintf('%04d-%02d-%02dT%02d:%02d:%02d', $year, $month, $day, intdiv($utcMinutes, 60), $utcMinutes % 60, $second)
            . ($fraction === '' ? '' : '.' . $fraction) . 'Z';
    }

    /** @return array{int, int, int} */
    private static function shiftDay(int $year, int $month, int $day, int $direction): array
    {
        if ($direction > 0) {
            ++$day;
            $days = self::daysInMonth($year, $month);
            if ($day > $days) {
                $day = 1;
                if (++$month > 12) {
                    $month = 1;
                    ++$year;
                }
            }
        } else {
            --$day;
            if ($day < 1) {
                if (--$month < 1) {
                    $month = 12;
                    --$year;
                }
                $day = self::daysInMonth($year, $month);
            }
        }
        return [$year, $month, $day];
    }

    private static function daysInMonth(int $year, int $month): int
    {
        if ($month === 2) {
            $leap = $year % 4 === 0 && ($year % 100 !== 0 || $year % 400 === 0);
            return $leap ? 29 : 28;
        }
        return in_array($month, [4, 6, 9, 11], true) ? 30 : 31;
    }

    /** @return array{int, int, int, int} */
    private static function normalizeMinutes(int $year, int $month, int $day, int $minutes): array
    {
        $days = intdiv($minutes, 1440);
        $minutes %= 1440;
        if ($minutes < 0) {
            --$days;
            $minutes += 1440;
        }
        $direction = $days <=> 0;
        while ($days !== 0) {
            [$year, $month, $day] = self::shiftDay($year, $month, $day, $direction);
            $days -= $direction;
        }
        return [$year, $month, $day, $minutes];
    }

    public function __toString(): string
    {
        return $this->value;
    }
}
