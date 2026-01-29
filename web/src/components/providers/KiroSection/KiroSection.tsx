import { Fragment } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import iconKiro from '@/assets/icons/kiro.svg';
import type { KiroAccount } from '@/types';
import styles from '@/pages/AiProvidersPage.module.scss';

interface KiroSectionProps {
  accounts: KiroAccount[];
  loading: boolean;
  error?: string | null;
  disableControls: boolean;
  onDelete: (index: number) => void;
  onRetryUsage: (index: number) => void;
  onRetryLoad?: () => void;
}

function getUsageBarColor(percentage: number): string {
  if (percentage >= 100) return 'var(--error-color)';
  if (percentage >= 80) return 'var(--warning-color)';
  return 'var(--success-color)';
}

function formatDate(dateStr: string): string {
  if (dateStr.includes('T')) {
    return dateStr.split('T')[0];
  }
  return dateStr;
}

export function KiroSection({
  accounts,
  loading,
  error: loadError,
  disableControls,
  onDelete,
  onRetryUsage,
  onRetryLoad,
}: KiroSectionProps) {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const handleGoAuth = () => {
    navigate('/oauth');
  };

  const renderAccountItem = (account: KiroAccount, index: number) => {
    const { usage, reset, subscription, loading: usageLoading, error, deleting } = account;

    return (
      <div key={account.name} className="item-row">
        <div className="item-meta">
          <div className="item-title">{account.email || account.name}</div>

          {usageLoading && (
            <div className="hint">{t('ai_providers.kiro_usage_loading')}</div>
          )}

          {error && !usageLoading && (
            <div className={styles.fieldRow}>
              <span className="status-badge error" style={{ marginRight: 8 }}>
                {t('ai_providers.kiro_usage_error')}
              </span>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => onRetryUsage(index)}
                disabled={disableControls}
              >
                {t('ai_providers.kiro_retry')}
              </Button>
            </div>
          )}

          {!usageLoading && !error && (
            <Fragment>
              {subscription && (
                <div className={styles.fieldRow}>
                  <span className={styles.fieldLabel}>{t('ai_providers.kiro_subscription')}:</span>
                  <span className={styles.fieldValue}>{subscription}</span>
                </div>
              )}

              {usage && (
                <Fragment>
                  <div className={styles.fieldRow}>
                    <span className={styles.fieldLabel}>{t('ai_providers.kiro_usage')}:</span>
                    <span className={styles.fieldValue}>
                      {usage.current} / {usage.limit} ({usage.percentage.toFixed(1)}%)
                    </span>
                  </div>
                  <div
                    className={styles.usageBarContainer}
                    role="progressbar"
                    aria-valuenow={Math.min(usage.percentage, 100)}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-label={`${t('ai_providers.kiro_usage')}: ${usage.percentage.toFixed(1)}%`}
                  >
                    <div
                      className={styles.usageBar}
                      style={{
                        width: `${Math.min(usage.percentage, 100)}%`,
                        backgroundColor: getUsageBarColor(usage.percentage),
                      }}
                    />
                  </div>
                </Fragment>
              )}

              {reset && (
                <div className={styles.fieldRow}>
                  <span className={styles.fieldLabel}>{t('ai_providers.kiro_reset')}:</span>
                  <span className={styles.fieldValue}>
                    {reset.daysUntil} {t('ai_providers.kiro_days')} ({formatDate(reset.nextDate)})
                  </span>
                </div>
              )}
            </Fragment>
          )}
        </div>
        <div className="item-actions">
          <Button
            variant="danger"
            size="sm"
            onClick={() => onDelete(index)}
            disabled={disableControls || deleting}
            loading={deleting}
          >
            {t('common.delete')}
          </Button>
        </div>
      </div>
    );
  };

  const renderContent = () => {
    if (loading) {
      return <div className="hint">{t('common.loading')}</div>;
    }

    if (loadError) {
      return (
        <div className={styles.fieldRow}>
          <span className="status-badge error" style={{ marginRight: 8 }}>
            {loadError}
          </span>
          {onRetryLoad && (
            <Button
              variant="secondary"
              size="sm"
              onClick={onRetryLoad}
              disabled={disableControls}
            >
              {t('ai_providers.kiro_retry')}
            </Button>
          )}
        </div>
      );
    }

    if (!accounts.length) {
      return (
        <EmptyState
          title={t('ai_providers.kiro_empty_title')}
          description={t('ai_providers.kiro_empty_desc')}
        />
      );
    }

    return <div className="item-list">{accounts.map(renderAccountItem)}</div>;
  };

  return (
    <Card
      title={
        <span className={styles.cardTitle}>
          <img src={iconKiro} alt="" className={styles.cardTitleIcon} />
          {t('ai_providers.kiro_title')}
        </span>
      }
      extra={
        <Button size="sm" onClick={handleGoAuth} disabled={disableControls}>
          {t('ai_providers.kiro_go_auth')}
        </Button>
      }
    >
      {renderContent()}
    </Card>
  );
}
