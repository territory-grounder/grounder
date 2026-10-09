# LibreNMS alert-dispatch logging (TG-336)

The LibreNMS host is not in this repository, so this file records a change made on `dc1nms01` — both
so it is reviewable and so the next person does not rediscover the problem it solves.

## The problem

```
*/2 * * * * librenms /opt/librenms/alerts.php >> /dev/null 2>&1
```

`alerts.php` prints its transport results to stdout. Cron discarded them. So there was **no record
anywhere** of whether a POST to TG's `/v1/ingest/librenms` succeeded, failed, timed out, or was never
attempted.

That mattered on 2026-08-05: LibreNMS logged 19 firing alerts and TG received zero `ingest_alert` rows,
and the reason could not be diagnosed from either side. TG's side showed nothing arriving; LibreNMS's side
showed alerts firing and rules correctly mapped to transport 7. The one thing that would have said which
end was at fault — the dispatcher's own output — was being thrown away every two minutes.

## The change

```
*/2 * * * * librenms /opt/librenms/alerts.php >> /var/log/librenms-alerts.log 2>&1
```

plus `/etc/logrotate.d/librenms-alerts` (daily, rotate 7, compress, copytruncate) so a verbose failure
mode cannot fill the disk — the reason someone reached for `/dev/null` in the first place, and the reason
it must not simply be reverted when the log grows.

The previous crontab is backed up on the host at `/root/librenms.cron.bak.<epoch>`.

## Why this is not just tidying

TG's own dead-man (TG-336) makes the SYMPTOM visible from TG's side: `tg_ingest_source_last_seen_seconds`
climbs against a non-zero baseline and `AlertSourceWentSilent` fires. That tells you a source stopped. It
cannot tell you WHY, because the answer lives in a process on another host that was writing its reasons to
`/dev/null`.

A dead-man that says "this stopped" and a log that says "here is what happened when it tried" are
different instruments, and the second one is what turns an alert into a fix.

## Still open

Why dispatched alerts on mapped rules do not arrive. The log now captures it; the next real firing alert
will show the transport result. Not root-caused at the time of writing.
