import React from 'react';
import clsx from 'clsx';
import styles from './styles.module.css';

const FeatureList = [
  {
    title: 'Physical Infrastructure',
    Svg: require('@site/static/img/undraw_server_status.svg').default,
    description: (
      <>
        Manage physical machine power states based on workload demands.
        Supports wake-on-LAN, IPMI, BMC interfaces, and smart PDUs.
      </>
    ),
  },
  {
    title: 'Kubernetes Native',
    Svg: require('@site/static/img/undraw_kubernetes.svg').default,
    description: (
      <>
        Uses Custom Resource Definitions and controllers for native integration.
        Webhook validation ensures data integrity across all resources.
      </>
    ),
  },
  {
    title: 'State Management',
    Svg: require('@site/static/img/undraw_server_cluster.svg').default,
    description: (
      <>
        Advanced finite state machine manages node power transitions.
        Coordination locks prevent race conditions during state changes.
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