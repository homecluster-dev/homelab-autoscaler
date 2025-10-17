import React from 'react';
import clsx from 'clsx';
import styles from './styles.module.css';

const FeatureList = [
  {
    title: 'Production Ready',
    Svg: require('@site/static/img/undraw_server_cluster.svg').default,
    description: (
      <>
        Advanced FSM-based state management with comprehensive testing infrastructure.
        Complete CloudProvider interface implementation for seamless Cluster Autoscaler integration.
      </>
    ),
  },
  {
    title: 'Physical Infrastructure',
    Svg: require('@site/static/img/undraw_server_status.svg').default,
    description: (
      <>
        Manage power states of physical machines based on workload demands.
        Support for Wake-on-LAN, IPMI, BMC interfaces, and smart PDUs.
      </>
    ),
  },
  {
    title: 'Kubernetes Native',
    Svg: require('@site/static/img/undraw_kubernetes.svg').default,
    description: (
      <>
        Built with Custom Resource Definitions and controllers.
        Webhook validation system ensuring data integrity across all resources.
      </>
    ),
  },
];

function Feature({Svg, title, description}) {
  return (
    <div className={clsx('col col--4')}>
      <div className="text--center">
        <Svg className={styles.featureSvg} role="img" />
      </div>
      <div className="text--center padding-horiz--md">
        <h3>{title}</h3>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures() {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}