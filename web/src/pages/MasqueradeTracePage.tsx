import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { IconRefreshCw, IconSearch, IconTrash2, IconX } from '@/components/ui/icons';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAuthStore, useNotificationStore } from '@/stores';
import {
  masqueradeTraceApi,
  type MasqueradeTraceSummary,
  type MasqueradeTraceRecord,
} from '@/services/api/masqueradeTrace';
import { formatUnixTimestamp } from '@/utils/format';
import styles from './MasqueradeTracePage.module.scss';

const getErrorMessage = (err: unknown): string => {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  if (typeof err !== 'object' || err === null) return '';
  if (!('message' in err)) return '';
  const message = (err as { message?: unknown }).message;
  return typeof message === 'string' ? message : '';
};

export function MasqueradeTracePage() {
  const { t } = useTranslation();
  const { showNotification, showConfirmation } = useNotificationStore();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);

  const [traces, setTraces] = useState<MasqueradeTraceSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [enabled, setEnabled] = useState(false);
  const [error, setError] = useState('');
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailRecord, setDetailRecord] = useState<MasqueradeTraceRecord | null>(null);

  const disableControls = connectionStatus !== 'connected';

  const loadTraces = async () => {
    if (connectionStatus !== 'connected') {
      setLoading(false);
      return;
    }
    setLoading(true);
    setError('');
    try {
      const data = await masqueradeTraceApi.fetchTraces();
      setTraces(data.traces || []);
      setEnabled(data.enabled);
    } catch (err: unknown) {
      console.error('Failed to load masquerade traces:', err);
      setError(getErrorMessage(err) || t('masquerade_trace.load_error'));
    } finally {
      setLoading(false);
    }
  };

  useHeaderRefresh(() => loadTraces());

  const clearTraces = async () => {
    showConfirmation({
      title: t('masquerade_trace.clear_confirm_title'),
      message: t('masquerade_trace.clear_confirm'),
      variant: 'danger',
      confirmText: t('common.confirm'),
      onConfirm: async () => {
        try {
          await masqueradeTraceApi.clearTraces();
          setTraces([]);
          showNotification(t('masquerade_trace.clear_success'), 'success');
        } catch (err: unknown) {
          const message = getErrorMessage(err);
          showNotification(
            `${t('notification.delete_failed')}${message ? `: ${message}` : ''}`,
            'error'
          );
        }
      },
    });
  };

  const openDetail = async (id: string) => {
    setSelectedId(id);
    setDetailLoading(true);
    setDetailRecord(null);
    try {
      const record = await masqueradeTraceApi.fetchTraceById(id);
      setDetailRecord(record);
    } catch (err: unknown) {
      console.error('Failed to load trace detail:', err);
      showNotification(getErrorMessage(err) || t('masquerade_trace.load_error'), 'error');
      setSelectedId(null);
    } finally {
      setDetailLoading(false);
    }
  };

  const closeDetail = () => {
    setSelectedId(null);
    setDetailRecord(null);
  };

  useEffect(() => {
    if (connectionStatus === 'connected') {
      loadTraces();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connectionStatus]);

  const trimmedSearchQuery = searchQuery.trim().toLowerCase();

  const filteredTraces = useMemo(() => {
    if (!trimmedSearchQuery) return traces;
    return traces.filter((trace) => {
      const searchableText = [
        trace.id,
        trace.model,
        trace.auth_id,
        trace.auth_label,
        trace.original_user_id,
        trace.masked_user_id,
      ]
        .join(' ')
        .toLowerCase();
      return searchableText.includes(trimmedSearchQuery);
    });
  }, [traces, trimmedSearchQuery]);

  const renderHeaderComparison = (
    original: Record<string, string> | undefined,
    masked: Record<string, string> | undefined
  ) => {
    const allKeys = new Set([
      ...Object.keys(original || {}),
      ...Object.keys(masked || {}),
    ]);
    const sortedKeys = Array.from(allKeys).sort();

    return (
      <div className={styles.comparisonGrid}>
        <div className={styles.comparisonColumn}>
          <div className={styles.comparisonHeader}>{t('masquerade_trace.original')}</div>
          <div className={styles.headerList}>
            {sortedKeys.map((key) => {
              const origVal = original?.[key];
              const maskedVal = masked?.[key];
              const changed = origVal !== maskedVal;
              return (
                <div
                  key={`orig-${key}`}
                  className={`${styles.headerItem} ${changed ? styles.headerChanged : ''}`}
                >
                  <span className={styles.headerKey}>{key}:</span>
                  <span className={styles.headerValue}>{origVal ?? <em>(absent)</em>}</span>
                </div>
              );
            })}
          </div>
        </div>
        <div className={styles.comparisonColumn}>
          <div className={styles.comparisonHeader}>{t('masquerade_trace.masked')}</div>
          <div className={styles.headerList}>
            {sortedKeys.map((key) => {
              const origVal = original?.[key];
              const maskedVal = masked?.[key];
              const changed = origVal !== maskedVal;
              return (
                <div
                  key={`masked-${key}`}
                  className={`${styles.headerItem} ${changed ? styles.headerChanged : ''}`}
                >
                  <span className={styles.headerKey}>{key}:</span>
                  <span className={styles.headerValue}>{maskedVal ?? <em>(absent)</em>}</span>
                </div>
              );
            })}
          </div>
        </div>
      </div>
    );
  };

  return (
    <div className={styles.container}>
      <h1 className={styles.pageTitle}>{t('masquerade_trace.title')}</h1>

      <Card className={styles.traceCard}>
        {error && <div className="error-box">{error}</div>}

        <div className={styles.filters}>
          <div className={styles.searchWrapper}>
            <Input
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              placeholder={t('masquerade_trace.search_placeholder')}
              className={styles.searchInput}
              rightElement={
                searchQuery ? (
                  <button
                    type="button"
                    className={styles.searchClear}
                    onClick={() => setSearchQuery('')}
                    title={t('common.close')}
                    aria-label={t('common.close')}
                  >
                    <IconX size={16} />
                  </button>
                ) : (
                  <IconSearch size={16} className={styles.searchIcon} />
                )
              }
            />
          </div>

          <div className={styles.statusBadge}>
            <span className={enabled ? styles.statusEnabled : styles.statusDisabled}>
              {enabled ? t('masquerade_trace.enabled') : t('masquerade_trace.disabled')}
            </span>
          </div>

          <div className={styles.toolbar}>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => loadTraces()}
              disabled={disableControls || loading}
              className={styles.actionButton}
            >
              <span className={styles.buttonContent}>
                <IconRefreshCw size={16} />
                {t('common.refresh')}
              </span>
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={clearTraces}
              disabled={disableControls || traces.length === 0}
              className={styles.actionButton}
            >
              <span className={styles.buttonContent}>
                <IconTrash2 size={16} />
                {t('masquerade_trace.clear_button')}
              </span>
            </Button>
          </div>
        </div>

        {loading ? (
          <div className="hint">{t('common.loading')}</div>
        ) : filteredTraces.length > 0 ? (
          <div className={styles.traceList}>
            <div className={styles.traceHeader}>
              <span className={styles.colTime}>{t('masquerade_trace.col_time')}</span>
              <span className={styles.colModel}>{t('masquerade_trace.col_model')}</span>
              <span className={styles.colAuth}>{t('masquerade_trace.col_auth')}</span>
              <span className={styles.colChanges}>{t('masquerade_trace.col_changes')}</span>
            </div>
            {filteredTraces.map((trace) => (
              <div
                key={trace.id}
                className={`${styles.traceRow} ${trace.user_id_changed ? styles.rowChanged : ''}`}
                onClick={() => openDetail(trace.id)}
              >
                <span className={styles.colTime}>
                  {trace.timestamp ? formatUnixTimestamp(trace.timestamp) : '-'}
                </span>
                <span className={styles.colModel} title={trace.model}>
                  {trace.model}
                </span>
                <span className={styles.colAuth} title={trace.auth_label || trace.auth_id}>
                  {trace.auth_label || trace.auth_id || '-'}
                </span>
                <span className={styles.colChanges}>
                  {trace.headers_modified > 0 && (
                    <span className={styles.changeBadge}>
                      {t('masquerade_trace.headers_modified', { count: trace.headers_modified })}
                    </span>
                  )}
                  {trace.user_id_changed && (
                    <span className={`${styles.changeBadge} ${styles.userIdBadge}`}>
                      {t('masquerade_trace.user_id_changed')}
                    </span>
                  )}
                </span>
              </div>
            ))}
          </div>
        ) : traces.length > 0 ? (
          <EmptyState
            title={t('masquerade_trace.search_empty_title')}
            description={t('masquerade_trace.search_empty_desc')}
          />
        ) : (
          <EmptyState
            title={t('masquerade_trace.empty_title')}
            description={t('masquerade_trace.empty_desc')}
          />
        )}
      </Card>

      <Modal
        open={Boolean(selectedId)}
        onClose={closeDetail}
        title={t('masquerade_trace.detail_title')}
        width={800}
      >
        {detailLoading ? (
          <div className="hint">{t('common.loading')}</div>
        ) : detailRecord ? (
          <div className={styles.detailContent}>
            <div className={styles.detailMeta}>
              <div className={styles.metaItem}>
                <span className={styles.metaLabel}>{t('masquerade_trace.col_time')}:</span>
                <span className={styles.metaValue}>
                  {detailRecord.timestamp ? formatUnixTimestamp(detailRecord.timestamp) : '-'}
                </span>
              </div>
              <div className={styles.metaItem}>
                <span className={styles.metaLabel}>{t('masquerade_trace.col_model')}:</span>
                <span className={styles.metaValue}>{detailRecord.model}</span>
              </div>
              <div className={styles.metaItem}>
                <span className={styles.metaLabel}>{t('masquerade_trace.col_auth')}:</span>
                <span className={styles.metaValue}>
                  {detailRecord.auth_label || detailRecord.auth_id || '-'}
                </span>
              </div>
              {detailRecord.hash_source && (
                <div className={styles.metaItem}>
                  <span className={styles.metaLabel}>{t('masquerade_trace.hash_source')}:</span>
                  <span className={styles.metaValue}>{detailRecord.hash_source}</span>
                </div>
              )}
            </div>

            <div className={styles.sectionTitle}>{t('masquerade_trace.user_id_section')}</div>
            <div className={styles.userIdComparison}>
              <div className={styles.userIdBlock}>
                <div className={styles.userIdLabel}>{t('masquerade_trace.original')}</div>
                <div className={styles.userIdValue}>{detailRecord.original_user_id || '-'}</div>
                {detailRecord.original_session && (
                  <div className={styles.sessionValue}>
                    Session: {detailRecord.original_session}
                  </div>
                )}
              </div>
              <div className={styles.userIdBlock}>
                <div className={styles.userIdLabel}>{t('masquerade_trace.masked')}</div>
                <div
                  className={`${styles.userIdValue} ${
                    detailRecord.original_user_id !== detailRecord.masked_user_id
                      ? styles.valueChanged
                      : ''
                  }`}
                >
                  {detailRecord.masked_user_id || '-'}
                </div>
                {detailRecord.masked_session && (
                  <div
                    className={`${styles.sessionValue} ${
                      detailRecord.original_session !== detailRecord.masked_session
                        ? styles.valueChanged
                        : ''
                    }`}
                  >
                    Session: {detailRecord.masked_session}
                  </div>
                )}
              </div>
            </div>

            <div className={styles.sectionTitle}>{t('masquerade_trace.headers_section')}</div>
            {renderHeaderComparison(detailRecord.original_headers, detailRecord.masked_headers)}
          </div>
        ) : null}
      </Modal>
    </div>
  );
}
